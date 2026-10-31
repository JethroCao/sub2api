package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestHomeWSUnbindStopsOldSocket(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, shadow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shadow=%t", mode, shadow), func(t *testing.T) {
				h, s := homeTransportSetup(t)
				a := homeTransportAccount()
				owner := a
				if shadow {
					owner = homeTransportAccount()
					owner.ID = 8
					a = &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID, Concurrency: 3}
				}
				repo := &homeMetadataRepo{accounts: map[int64]*Account{owner.ID: owner, a.ID: a}}
				s.accountRepo = repo
				s.homeCredentials.accounts = repo
				var sends atomic.Int32
				client, _ := homeWSSession(t, mode, h, s, func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
					conn, e := coderws.Accept(w, r, nil)
					if e != nil {
						return
					}
					defer conn.CloseNow()
					for {
						_, _, e = conn.Read(r.Context())
						if e != nil {
							return
						}
						sends.Add(1)
						_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_done","status":"completed"}}`))
					}
				}, a)
				homeWSWrite(t, client)
				_, err := homeWSRead(t, client)
				require.NoError(t, err)
				transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
				native := *owner
				native.Extra = nil
				native.Credentials = map[string]any{"access_token": "native-new-token", "chatgpt_account_id": "native-new-account"}
				repo.replace(&native)
				homeWSWrite(t, client)
				_, err = homeWSRead(t, client)
				require.Error(t, err, "an existing Home-authenticated socket must not execute after ownership is removed")
				require.Equal(t, int32(1), sends.Load(), "second request must not use old Home identity")
			})
		}
	}
}

func TestHomeWSDialFailurePreservesHomeOutcome(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, "forwarder"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", mode, status), func(t *testing.T) {
				h, s := homeTransportSetup(t)
				sessionMode := mode
				if mode == "forwarder" {
					sessionMode = OpenAIWSIngressModeCtxPool
				}
				var dials atomic.Int32
				client, serverErr := homeWSSession(t, sessionMode, h, s, func(w http.ResponseWriter, r *http.Request) {
					dials.Add(1)
					require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(status)
					if status == http.StatusUnauthorized {
						_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"access token invalid"}}`)
					} else {
						_, _ = io.WriteString(w, `{"error":{"code":"usage_limit_reached"}}`)
					}
				})
				if mode == "forwarder" {
					_ = client.CloseNow()
					c := newOpenAICompactFallbackTestContext(t, "/v1/responses")
					_, err := s.forwardOpenAIWSV2(context.Background(), c, homeTransportAccount(), map[string]any{"model": transportHomeTestModel, "stream": false, "input": []any{}}, "", "", "stale-token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, false, transportHomeTestModel, transportHomeTestModel, time.Now(), 1, "", new(bool))
					require.Error(t, err)
				} else {
					homeWSWrite(t, client)
					select {
					case err := <-serverErr:
						require.Error(t, err)
					case <-time.After(3 * time.Second):
						t.Fatal("dial did not end")
					}
				}
				admitted := mode != OpenAIWSIngressModeCtxPool
				if admitted {
					transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
					h.mu.Lock()
					var report struct {
						Failed bool `json:"failed"`
						Fail   struct {
							Status int    `json:"status_code"`
							Body   string `json:"body"`
						} `json:"fail"`
					}
					for _, r := range h.records {
						if r.args[0] == "LPUSH" && r.args[1] == "usage" {
							require.NoError(t, json.Unmarshal([]byte(r.args[2]), &report))
						}
					}
					h.mu.Unlock()
					require.True(t, report.Failed)
					require.Equal(t, status, report.Fail.Status)
					if status == http.StatusTooManyRequests {
						require.Contains(t, report.Fail.Body, `"resets_in_seconds":2`)
					}
				} else {
					require.Zero(t, h.count("RPOP", ""))
					require.Zero(t, h.count("LPUSH", "usage"))
				}
				if status == http.StatusUnauthorized {
					sum := sha256.Sum256([]byte("access-token"))
					require.Eventually(t, func() bool {
						h.mu.Lock()
						defer h.mu.Unlock()
						for _, r := range h.records {
							if r.args[0] == "GET" && r.args[1] != "config" {
								var req map[string]string
								_ = json.Unmarshal([]byte(r.args[1]), &req)
								if req["auth_index"] == transportHomeTestID && req["access_token_sha256"] == hex.EncodeToString(sum[:]) {
									return true
								}
							}
						}
						return false
					}, time.Second, 5*time.Millisecond, "401 must recover the exact handshake AT")
				}
				require.Equal(t, int32(1), dials.Load(), "recovery must not replay the rejected handshake/model attempt")
			})
		}
	}
}

// Keep the real socket/reader behavior; inspect pool ownership when the physical
// abort happens, so restoring the wrong defer order makes this test fail.
type homeCloseOrderingDialer struct {
	inner       openAIWSClientDialer
	beforeClose func()
}

func (d homeCloseOrderingDialer) Dial(ctx context.Context, url string, h http.Header, p string) (openAIWSClientConn, int, http.Header, error) {
	c, status, headers, err := d.inner.Dial(ctx, url, h, p)
	if err != nil {
		return c, status, headers, err
	}
	return &homeCloseOrderingConn{openAIWSClientConn: c, beforeClose: d.beforeClose}, status, headers, nil
}

type homeCloseOrderingConn struct {
	openAIWSClientConn
	beforeClose func()
}

func (c *homeCloseOrderingConn) CloseNow() error {
	c.beforeClose()
	return c.openAIWSClientConn.(openAIWSForceCloser).CloseNow()
}
func TestHomeWSFailedForwarderAbortsBeforePoolRelease(t *testing.T) {
	h, s := homeTransportSetup(t)
	spare, _ := homeWSSession(t, OpenAIWSIngressModeCtxPool, h, s, func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"server_error"}}}`))
		_, _, _ = conn.Read(r.Context())
	})
	_ = spare.CloseNow()
	var closeWhileOwned atomic.Bool
	var closeSeen atomic.Bool
	s.openaiWSPool.setClientDialerForTest(homeCloseOrderingDialer{inner: s.openaiWSPool.clientDialer, beforeClose: func() {
		ap, ok := s.openaiWSPool.getAccountPool(7)
		if !ok {
			return
		}
		ap.mu.Lock()
		defer ap.mu.Unlock()
		for _, conn := range ap.conns {
			closeSeen.Store(true)
			closeWhileOwned.Store(len(conn.leaseCh) == 0)
		}
	}})
	c := newOpenAICompactFallbackTestContext(t, "/v1/responses")
	_, err := s.forwardOpenAIWSV2(context.Background(), c, homeTransportAccount(), map[string]any{"model": transportHomeTestModel, "stream": false, "input": []any{}}, "", "", "stale-token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, false, transportHomeTestModel, transportHomeTestModel, time.Now(), 1, "", new(bool))
	require.NoError(t, err)
	require.True(t, closeSeen.Load(), "failed turn must close its actual upstream socket")
	require.True(t, closeWhileOwned.Load(), "old Home turn must abort before the pool can lease its socket to another turn")
}

func TestHomeWSDelayedShadowDialCannotReuseUnboundHeaders(t *testing.T) {
	h, s := homeTransportSetup(t)
	owner := homeTransportAccount()
	shadow := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID, Concurrency: 3}
	repo := &homeMetadataRepo{accounts: map[int64]*Account{owner.ID: owner, shadow.ID: shadow}}
	s.accountRepo = repo
	s.homeCredentials.accounts = repo
	var dials atomic.Int32
	client, _ := homeWSSession(t, OpenAIWSIngressModeCtxPool, h, s, func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_shadow","status":"completed"}}`))
		_, _, _ = conn.Read(r.Context())
	}, shadow)
	homeWSWrite(t, client)
	_, err := homeWSRead(t, client)
	require.NoError(t, err)
	ap, ok := s.openaiWSPool.getAccountPool(shadow.ID)
	require.True(t, ok)
	ap.mu.Lock()
	require.NotNil(t, ap.lastAcquire)
	delayed := cloneOpenAIWSAcquireRequest(*ap.lastAcquire)
	ap.mu.Unlock()
	native := *owner
	native.Extra = nil
	native.Credentials = map[string]any{"access_token": "native-new-token", "chatgpt_account_id": "native-new-account"}
	repo.replace(&native)
	conn, err := s.openaiWSPool.dialConn(context.Background(), delayed)
	if conn != nil {
		conn.abort()
	}
	require.Error(t, err, "a delayed shadow prewarm must retain the provenance of its saved Home headers")
	require.Equal(t, int32(1), dials.Load(), "stale Home token must not be used even for an idle handshake")
}
