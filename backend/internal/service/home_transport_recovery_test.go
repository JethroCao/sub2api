package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

const homeRecoveryOldID = "c33f8376-69d7-4fbf-81fa-d2a4fc8ea7ba"

// The initial ownership check sees A; Acquire's fresh read sees B. Shadow
// lookups retain the local ID and resolve the parent at each boundary.
type homeRecoveryRebindRepo struct {
	AccountRepository
	mu                         sync.Mutex
	before, admitted, selected *Account
	ownerReads                 int
}

func (r *homeRecoveryRebindRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.selected.IsCredentialShadow() && id == r.selected.ID {
		return r.selected, nil
	}
	if id != r.admitted.ID {
		return nil, ErrAccountNotFound
	}
	r.ownerReads++
	if r.ownerReads == 1 {
		return r.before, nil
	}
	return r.admitted, nil
}

func homeRecoveryRebindSetup(t *testing.T, shadow bool) (*transportHomeFakeHome, *OpenAIGatewayService, *Account) {
	t.Helper()
	h, s := homeTransportSetup(t)
	before := homeTransportAccount()
	before.Extra[HomeCredentialIDKey] = homeRecoveryOldID
	admitted := homeTransportAccount()
	selected := before
	persisted := before
	if shadow {
		before.ID, admitted.ID = 8, 8
		parentID := int64(8)
		persisted = &Account{ID: 7, ParentAccountID: &parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 3}
		selected = ApplyHomeCredential(persisted, before, homeclient.Credential{ID: homeRecoveryOldID, AccessToken: "stale-token", AccountID: "stale-account"})
	}
	repo := &homeRecoveryRebindRepo{before: before, admitted: admitted, selected: persisted}
	s.accountRepo = repo
	s.homeCredentials = NewHomeCredentialsService(repo, s.homeCredentials.client)
	return h, s, selected
}

func requireHomeRecoveryOfAdmittedCredential(t *testing.T, h *transportHomeFakeHome) {
	t.Helper()
	transportHomeEventually(t, func() bool { return h.count("GET", "") >= 2 }) // config plus refresh
	sum := sha256.Sum256([]byte("access-token"))
	h.mu.Lock()
	defer h.mu.Unlock()
	var refreshes int
	for _, rec := range h.records {
		if rec.args[0] != "GET" || rec.args[1] == "config" {
			continue
		}
		refreshes++
		var request map[string]string
		require.NoError(t, json.Unmarshal([]byte(rec.args[1]), &request))
		require.Equal(t, transportHomeTestID, request["auth_index"])
		require.Equal(t, hex.EncodeToString(sum[:]), request["access_token_sha256"])
	}
	require.Equal(t, 1, refreshes)
}

func TestHomeHTTPRecoveryUsesAdmittedBindingAfterRebind(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "shadow"}[shadow], func(t *testing.T) {
			h, s, selected := homeRecoveryRebindSetup(t, shadow)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
			}))
			defer upstream.Close()
			response, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", selected)
			require.NoError(t, err)
			_, err = io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			requireHomeRecoveryOfAdmittedCredential(t, h)
			transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
			require.Equal(t, 1, h.count("RPOP", ""), "recovery must not replay the model request")
			require.Equal(t, homeRecoveryOldID, selected.Extra[HomeCredentialIDKey], "selected routing snapshot must stay unchanged")
			require.Equal(t, int64(7), selected.ID)
		})
	}
}

func TestHomeWSRecoveryUsesAdmittedBindingAfterRebind(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "shadow"}[shadow], func(t *testing.T) {
			h, s, selected := homeRecoveryRebindSetup(t, shadow)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			turn, err := s.beginHomeWSTurn(ctx, selected, transportHomeTestModel)
			require.NoError(t, err)
			defer turn.finish()
			stopped := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, _, err = conn.Read(ctx)
				if err != nil {
					return
				}
				_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"error","message":"authentication_error"}`))
				_, _, _ = conn.Read(ctx)
				close(stopped)
			}))
			defer upstream.Close()
			headers := make(http.Header)
			applyHomeHeaders(headers, turn.lease.Credential)
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"), &coderws.DialOptions{HTTPHeader: headers})
			require.NoError(t, err)
			defer conn.CloseNow()
			frames := &homeWSFrameConn{inner: &openAIWSClientFrameConn{conn: conn}, svc: s, account: selected, controlCtx: ctx, identity: homeHeaderIdentity(headers), stop: func() { _ = conn.CloseNow() }, first: turn}
			defer frames.Close()
			require.NoError(t, frames.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1-codex"}`)))
			_, payload, err := frames.ReadFrame(ctx)
			require.NoError(t, err)
			require.JSONEq(t, `{"type":"error","message":"authentication_error"}`, string(payload))
			requireHomeRecoveryOfAdmittedCredential(t, h)
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("failed upstream WebSocket remained open")
			}
			transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
			require.Equal(t, 1, h.count("RPOP", ""))
			require.Equal(t, homeRecoveryOldID, selected.Extra[HomeCredentialIDKey])
			require.Equal(t, int64(7), selected.ID)
		})
	}
}

func TestHomeAttemptRecoveryRejectsBindingChangedAfterAdmission(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "shadow"}[shadow], func(t *testing.T) {
			h, s, selected := homeRecoveryRebindSetup(t, shadow)
			turn, err := s.beginHomeWSTurn(context.Background(), selected, transportHomeTestModel)
			require.NoError(t, err)
			defer turn.finish()
			repo := s.accountRepo.(*homeRecoveryRebindRepo)
			// Rebinding B back to A after the model send must not refresh A using
			// B's rejected token. This also checks shadow-parent ownership rereads.
			repo.mu.Lock()
			repo.admitted = repo.before
			repo.mu.Unlock()
			err = s.homeCredentials.Recover(context.Background(), turn.account, turn.lease.Credential.AccessToken)
			require.ErrorIs(t, err, ErrHomeOwnershipChanged)
			require.Equal(t, 1, h.count("GET", ""), "only initial config fetch is allowed, never credential refresh")
		})
	}
}

func TestHomeHTTPProjectedAuthMessageTriggersRecovery(t *testing.T) {
	for _, size := range []int{0, homeObserveLimit + 100} {
		h, s := homeTransportSetup(t)
		payload := "data: " + `{"type":"error","output":"` + strings.Repeat("x", size) + `","message":"authentication_error"}` + "\n\ndata: [DONE]\n\n"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, payload)
		}))
		response, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		upstream.Close()
		require.True(t, string(body) == payload, "projection must not alter upstream bytes")
		requireHomeRecoveryOfAdmittedCredential(t, h)
		require.Equal(t, 1, h.count("RPOP", ""))
	}
}
