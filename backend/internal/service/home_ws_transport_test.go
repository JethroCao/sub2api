package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// Redirect only the dial destination: the real WebSocket transport, header
// handshake, ingress and relay all run against a local upstream server.
type homeLocalWSDialer struct {
	url   string
	inner openAIWSClientDialer
}

func (d homeLocalWSDialer) Dial(ctx context.Context, _ string, h http.Header, p string) (openAIWSClientConn, int, http.Header, error) {
	return d.inner.Dial(ctx, d.url, h, p)
}
func TestHomeWSEveryTurnIsAdmitted(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeDedicated, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 10
			account := homeTransportAccount()
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			var sends atomic.Int32
			headers := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					_, _, err = conn.Read(r.Context())
					if err != nil {
						return
					}
					sends.Add(1)
					err = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1-codex","usage":{"input_tokens":1,"output_tokens":1}}}`))
					if err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			dialer := homeLocalWSDialer{"ws" + strings.TrimPrefix(upstream.URL, "http"), newDefaultOpenAIWSClientDialer()}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			s.cfg = cfg
			s.cache = &stubGatewayCache{}
			s.openaiWSResolver = NewOpenAIWSProtocolResolver(cfg)
			s.toolCorrector = NewCodexToolCorrector()
			s.openaiWSPool = pool
			s.openaiWSPassthroughDialer = dialer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server, serverErr := startPassthroughLifecycleServer(t, ctx, s, account)
			defer server.Close()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			request := []byte(`{"type":"response.create","model":"gpt-5.1-codex","stream":true}`)
			require.NoError(t, client.Write(ctx, coderws.MessageText, request))
			readCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			_, body, err := client.Read(readCtx)
			stop()
			if err != nil {
				select {
				case serverError := <-serverErr:
					t.Fatalf("read failed %v, server: %v", err, errors.Unwrap(serverError))
				default:
				}
			}
			require.NoError(t, err)
			require.Contains(t, string(body), "response.completed")
			hdr := <-headers
			require.Equal(t, "Bearer access-token", hdr.Get("Authorization"))
			require.Equal(t, "account", hdr.Get("Chatgpt-Account-Id"))
			transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
			held := []*homeclient.Lease{}
			for i := 0; i < 3; i++ {
				lease, e := s.homeCredentials.Acquire(ctx, account, homeclient.Request{Model: transportHomeTestModel})
				require.NoError(t, e)
				held = append(held, lease)
			}
			defer func() {
				for _, lease := range held {
					lease.Finish(homeclient.Result{})
				}
			}()
			require.NoError(t, client.Write(ctx, coderws.MessageText, request))
			select {
			case err := <-serverErr:
				require.Error(t, err)
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, err, &closeErr)
				require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
			case <-time.After(6 * time.Second):
				t.Fatal("second turn was not refused")
			}
			require.Equal(t, int32(1), sends.Load())
			require.Equal(t, 5, h.count("RPOP", ""))
		})
	}
}

func homeWSSession(t *testing.T, mode string, home *transportHomeFakeHome, s *OpenAIGatewayService, handler http.HandlerFunc, selected ...*Account) (*coderws.Conn, <-chan error) {
	t.Helper()
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 10
	a := homeTransportAccount()
	if len(selected) > 0 {
		copy := *selected[0]
		copy.Extra = shallowCopyMap(copy.Extra)
		if copy.Extra == nil {
			copy.Extra = make(map[string]any)
		}
		a = &copy
	}
	a.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	dialer := homeLocalWSDialer{"ws" + strings.TrimPrefix(upstream.URL, "http"), newDefaultOpenAIWSClientDialer()}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	s.cfg = cfg
	s.cache = &stubGatewayCache{}
	s.openaiWSResolver = NewOpenAIWSProtocolResolver(cfg)
	s.toolCorrector = NewCodexToolCorrector()
	s.openaiWSPool = pool
	s.openaiWSPassthroughDialer = dialer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server, serverErr := startPassthroughLifecycleServer(t, ctx, s, a)
	t.Cleanup(server.Close)
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { client.CloseNow() })
	return client, serverErr
}
func homeWSWrite(t *testing.T, c *coderws.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1-codex","stream":true}`)))
}
func homeWSRead(t *testing.T, c *coderws.Conn) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, b, e := c.Read(ctx)
	return b, e
}
func TestHomeWSCredentialRotationNeverUsesOldSocket(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			sent := make(chan string, 4)
			c, serverErr := homeWSSession(t, mode, h, s, func(w http.ResponseWriter, r *http.Request) {
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
					sent <- r.Header.Get("Authorization")
					_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_rotate","status":"completed"}}`))
				}
			})
			homeWSWrite(t, c)
			_, e := homeWSRead(t, c)
			require.NoError(t, e)
			require.Equal(t, "Bearer access-token", <-sent)
			transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
			h.mu.Lock()
			h.mutate = func(reply map[string]any) {
				reply["auth"].(map[string]any)["metadata"].(map[string]any)["access_token"] = "rotated-token"
			}
			h.mu.Unlock()
			homeWSWrite(t, c)
			_, e = homeWSRead(t, c)
			if mode == OpenAIWSIngressModeCtxPool {
				require.NoError(t, e)
				require.Equal(t, "Bearer rotated-token", <-sent)
				require.Equal(t, 3, h.count("RPOP", ""))
			} else {
				require.Error(t, e)
				select {
				case token := <-sent:
					t.Fatalf("rotation sent through old socket: %s", token)
				default:
				}
				select {
				case err := <-serverErr:
					require.Error(t, err)
				case <-time.After(time.Second):
					t.Fatal("rotation did not terminate")
				}
			}
		})
	}
}
func TestHomeWSMembershipLossClosesActiveTransport(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			sent := make(chan struct{})
			stopped := make(chan struct{})
			c, _ := homeWSSession(t, mode, h, s, func(w http.ResponseWriter, r *http.Request) {
				conn, e := coderws.Accept(w, r, nil)
				if e != nil {
					return
				}
				defer conn.CloseNow()
				_, _, e = conn.Read(r.Context())
				if e != nil {
					return
				}
				close(sent)
				_, _, _ = conn.Read(r.Context())
				close(stopped)
			})
			homeWSWrite(t, c)
			select {
			case <-sent:
			case <-time.After(time.Second):
				t.Fatal("first frame not sent")
			}
			require.Zero(t, h.count("LPUSH", "concurrency-release"))
			h.mu.Lock()
			h.heartbeat = false
			h.mu.Unlock()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream survived Home membership loss")
			}
			transportHomeEventually(t, func() bool {
				h.mu.Lock()
				defer h.mu.Unlock()
				n := 0
				for _, v := range h.active {
					n += v
				}
				return n == 0
			})
		})
	}
}

func TestHomeWSRetryAcquiresAgainAndReportsFailure(t *testing.T) {
	h, s := homeTransportSetup(t)
	var sends atomic.Int32
	client, _ := homeWSSession(t, OpenAIWSIngressModeCtxPool, h, s, func(w http.ResponseWriter, r *http.Request) {
		conn, e := coderws.Accept(w, r, nil)
		if e != nil {
			return
		}
		defer conn.CloseNow()
		_, _, e = conn.Read(r.Context())
		if e != nil {
			return
		}
		if sends.Add(1) == 1 {
			return
		}
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_retry","status":"completed"}}`))
		_, _, _ = conn.Read(r.Context())
	})
	homeWSWrite(t, client)
	body, e := homeWSRead(t, client)
	require.NoError(t, e)
	require.Contains(t, string(body), "resp_retry")
	require.Equal(t, int32(2), sends.Load())
	require.Equal(t, 2, h.count("RPOP", ""))
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 2 })
	h.mu.Lock()
	defer h.mu.Unlock()
	failed := 0
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "usage" && strings.Contains(r.args[2], `"failed":true`) {
			failed++
		}
	}
	require.Equal(t, 1, failed)
}

func TestHomeWSForwarderDisablesOnlyOptionalGeneratePrewarm(t *testing.T) {
	h, s := homeTransportSetup(t)
	var sends atomic.Int32
	client, _ := homeWSSession(t, OpenAIWSIngressModeCtxPool, h, s, func(w http.ResponseWriter, r *http.Request) {
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
			_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_forwarder","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`))
		}
	})
	// This spare downstream connection has sent nothing; exercise HTTP->WS
	// forwarder directly against the same real local upstream dialer instead.
	client.CloseNow()
	s.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
	ctx := context.Background()
	c := newOpenAICompactFallbackTestContext(t, "/v1/responses")
	result, err := s.forwardOpenAIWSV2(ctx, c, homeTransportAccount(), map[string]any{"model": "gpt-5.1-codex", "stream": false, "input": []any{}}, "", "", "stale-token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, false, "gpt-5.1-codex", "gpt-5.1-codex", time.Now(), 1, "", new(bool))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int32(1), sends.Load())
	require.Equal(t, 1, h.count("RPOP", ""))
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
}

func TestHomeWSCancelFrameRetainsAdmissionUntilTerminal(t *testing.T) {
	h, s := homeTransportSetup(t)
	sent := make(chan struct{})
	cancelSeen := make(chan struct{})
	finish := make(chan struct{})
	client, _ := homeWSSession(t, OpenAIWSIngressModePassthrough, h, s, func(w http.ResponseWriter, r *http.Request) {
		conn, e := coderws.Accept(w, r, nil)
		if e != nil {
			return
		}
		defer conn.CloseNow()
		_, _, e = conn.Read(r.Context())
		if e != nil {
			return
		}
		close(sent)
		// The passthrough reader starts client forwarding after first downstream output.
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_cancel"}}`))
		_, b, e := conn.Read(r.Context())
		if e != nil {
			return
		}
		if strings.Contains(string(b), "response.cancel") {
			close(cancelSeen)
		}
		<-finish
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_cancel","status":"completed"}}`))
		_, _, _ = conn.Read(r.Context())
	})
	homeWSWrite(t, client)
	<-sent
	_, e := homeWSRead(t, client)
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.cancel"}`)))
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("cancel frame not forwarded")
	}
	require.Equal(t, 1, h.count("RPOP", ""))
	require.Zero(t, h.count("LPUSH", "concurrency-release"))
	close(finish)
	_, e = homeWSRead(t, client)
	require.NoError(t, e)
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
}

func TestHomeWSHTTPBridgeUsesOneHTTPAdmissionPerTurn(t *testing.T) {
	h, s := homeTransportSetup(t)
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bridge_home\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	client, serverErr := homeWSSession(t, OpenAIWSIngressModeHTTPBridge, h, s, func(w http.ResponseWriter, r *http.Request) { t.Error("HTTP bridge must not dial a WS upstream") })
	s.httpUpstream = homeRedirectHTTP{upstream.URL}
	homeWSWrite(t, client)
	body, err := homeWSRead(t, client)
	require.NoError(t, err)
	require.Contains(t, string(body), "resp_bridge_home")
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
	require.Equal(t, 1, h.count("RPOP", ""))
	held := []*homeclient.Lease{}
	for i := 0; i < 3; i++ {
		lease, e := s.homeCredentials.Acquire(context.Background(), homeTransportAccount(), homeclient.Request{Model: transportHomeTestModel})
		require.NoError(t, e)
		held = append(held, lease)
	}
	defer func() {
		for _, l := range held {
			l.Finish(homeclient.Result{})
		}
	}()
	homeWSWrite(t, client)
	_, err = homeWSRead(t, client)
	require.Error(t, err)
	select {
	case err := <-serverErr:
		var closeErr *OpenAIWSClientCloseError
		require.ErrorAs(t, err, &closeErr)
		require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
	case <-time.After(time.Second):
		t.Fatal("HTTP bridge denial did not terminate")
	}
	require.Equal(t, int32(1), sends.Load())
	require.Equal(t, 5, h.count("RPOP", ""))
}
