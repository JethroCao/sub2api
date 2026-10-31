package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/stretchr/testify/require"
)

func TestHomeHTTPUnbindCannotDispatchPreparedHomeIdentity(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "shadow"}[shadow], func(t *testing.T) {
			h, s := homeTransportSetup(t)
			owner := homeTransportAccount()
			selected := owner
			if shadow {
				owner.ID = 8
				selected = &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID, Concurrency: 3}
			}
			repo := &homeMetadataRepo{accounts: map[int64]*Account{owner.ID: owner, selected.ID: selected}}
			s.accountRepo = repo
			s.homeCredentials.accounts = repo
			ctx, token, _, err := s.getOpenAIRequestAccessToken(t.Context(), nil, selected)
			require.NoError(t, err)
			require.Equal(t, "access-token", token)
			var sends atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				_, _ = io.WriteString(w, `{"status":"completed"}`)
			}))
			defer upstream.Close()
			request := homeTransportRequest(t, upstream.URL).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Chatgpt-Account-Id", "account")
			native := *owner
			native.Extra = nil
			native.Credentials = map[string]any{"access_token": "independent-native-token", "chatgpt_account_id": "independent-native-account"}
			repo.replace(&native)
			response, err := s.doOpenAIUpstream(request, "", selected)
			if response != nil {
				response.Body.Close()
			}
			require.Error(t, err, "a Home-built request cannot fall through to native dispatch after unbinding")
			require.Zero(t, sends.Load())
			require.Zero(t, h.count("RPOP", ""))
		})
	}
}

func TestHomeWSFailureKeepsShadowAttemptContext(t *testing.T) {
	_, s := homeTransportSetup(t)
	owner := homeTransportAccount()
	shadow := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID}
	repo := &homeMetadataRepo{accounts: map[int64]*Account{owner.ID: owner, shadow.ID: shadow}}
	s.accountRepo = repo
	s.homeCredentials.accounts = repo
	ctx, _, _, err := s.getOpenAIRequestAccessToken(t.Context(), nil, shadow)
	require.NoError(t, err)
	native := *owner
	native.Extra = nil
	native.Credentials = map[string]any{"access_token": "new-native-token"}
	repo.replace(&native)
	s.rateLimitService = NewRateLimitService(repo, nil, nil, nil, nil)
	s.rateLimitService.homeCredentials = s.homeCredentials
	s.handleOpenAIWSFailureAccountSideEffects(ctx, shadow, transportHomeTestModel, nil, []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"token_revoked","status_code":401}}}`))
	require.Zero(t, repo.errors.Load(), "WS terminal side effects must retain Home provenance through the Gin-free helper")
}

func TestHomeHTTPPreparedBindingCannotChangeAtAdmission(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_admission", true: "during_admission"}[during], func(t *testing.T) {
			h, s := homeTransportSetup(t)
			account := homeTransportAccount()
			repo := &homeMetadataRepo{accounts: map[int64]*Account{account.ID: account}}
			s.accountRepo = repo
			s.homeCredentials.accounts = repo
			ctx, _, _, err := s.getOpenAIRequestAccessToken(t.Context(), nil, account)
			require.NoError(t, err)
			next := homeTransportAccount()
			next.Extra[HomeCredentialIDKey] = homeRecoveryOldID
			h.mu.Lock()
			h.mutate = func(reply map[string]any) {
				h.mu.Lock()
				var requested struct {
					ID string `json:"pinned_auth_id"`
				}
				for i := len(h.records) - 1; i >= 0; i-- {
					if h.records[i].args[0] == "RPOP" {
						_ = json.Unmarshal([]byte(h.records[i].args[1]), &requested)
						break
					}
				}
				h.mu.Unlock()
				reply["auth_index"] = requested.ID
				reply["auth"].(map[string]any)["id"] = requested.ID
				reply["concurrency"].(map[string]any)["credential_id"] = requested.ID
			}
			h.mu.Unlock()
			if during {
				sequence := &homeRecoveryRebindRepo{before: account, admitted: next, selected: account}
				s.accountRepo = sequence
				s.homeCredentials.accounts = sequence
			} else {
				repo.replace(next)
			}
			lease, err := beginHomeAttempt(ctx, s.homeCredentials, account, transportHomeTestModel)
			if lease != nil {
				lease.Finish(homeclient.Result{})
			}
			require.Error(t, err, "a prepared request must not silently change UUID at the dispatch boundary")
			require.Nil(t, lease)
			if during {
				require.Equal(t, 1, h.count("RPOP", ""))
				transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 && h.count("LPUSH", "usage") == 1 })
				h.mu.Lock()
				defer h.mu.Unlock()
				for _, record := range h.records {
					if record.args[0] == "LPUSH" && record.args[1] == "usage" {
						require.Contains(t, record.args[2], `"status_code":499`)
					}
				}
			} else {
				require.Zero(t, h.count("RPOP", ""))
			}
		})
	}
}
