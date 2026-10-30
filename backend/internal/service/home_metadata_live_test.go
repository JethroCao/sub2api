package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type homeMetadataRepo struct {
	AccountRepository
	mu       sync.Mutex
	accounts map[int64]*Account
	errors   atomic.Int32
}

func (r *homeMetadataRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.accounts[id]; a != nil {
		return a, nil
	}
	return nil, ErrAccountNotFound
}
func (r *homeMetadataRepo) replace(a *Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[a.ID] = a
}
func (r *homeMetadataRepo) SetError(context.Context, int64, string) error {
	r.errors.Add(1)
	return nil
}

type homeMetadataClient struct {
	homeAuxiliaryClient
	acquires atomic.Int32
}

func (c *homeMetadataClient) Acquire(context.Context, homeclient.Request) (*homeclient.Lease, error) {
	c.acquires.Add(1)
	return nil, errors.New("metadata must not acquire an inference slot")
}

type homeMetadataUpstream struct {
	send func(*http.Request) (*http.Response, error)
}

func (h *homeMetadataUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.send(r)
}
func (h *homeMetadataUpstream) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.send(r)
}

func TestHomeModelMetadata401UsesActualSnapshot(t *testing.T) {
	for _, caller := range []string{"admin", "public", "admin_shadow", "public_shadow"} {
		for _, scenario := range []string{"recover", "rotated", "rebound", "unbound"} {
			t.Run(caller+"/"+scenario, func(t *testing.T) {
				account := homeTestAccount()
				account.Credentials = map[string]any{}
				repo := &homeMetadataRepo{accounts: map[int64]*Account{7: account}}
				selected := account
				if strings.HasSuffix(caller, "_shadow") {
					selected = &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &account.ID}
					repo.replace(selected)
				}
				client := &homeMetadataClient{}
				client.refresh = func(id, observed string) (homeclient.Credential, error) {
					token := "actual-initial-at"
					if observed != "" {
						token = "recovered-at"
					}
					return homeclient.Credential{ID: id, AccessToken: token, AccountID: "actual-workspace", ExpiresAt: time.Now().Add(time.Hour)}, nil
				}
				home := NewHomeCredentialsService(repo, client)
				var requests atomic.Int32
				var lastAuthorization atomic.Value
				send := func(r *http.Request) (*http.Response, error) {
					lastAuthorization.Store(r.Header.Get("Authorization"))
					if r.Header.Get("Chatgpt-Account-Id") != "actual-workspace" {
						t.Error("model request lost the prepared Home account identity")
					}
					status, body := http.StatusOK, `{"models":[{"slug":"gpt-test"}]}`
					if requests.Add(1) == 1 {
						switch scenario {
						case "rotated":
							if err := home.cacheCredential(testHomeID, homeclient.Credential{ID: testHomeID, AccessToken: "rotated-at", AccountID: "actual-workspace", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
								t.Error(err)
							}
						case "rebound", "unbound":
							updated := homeTestAccount()
							updated.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
							if scenario == "unbound" {
								updated.Extra = nil
								updated.Credentials = map[string]any{"access_token": "native-at"}
							}
							repo.replace(updated)
						}
						status, body = http.StatusUnauthorized, `{"error":{"code":"token_revoked"}}`
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}
				var invoke func() error
				if strings.HasPrefix(caller, "admin") {
					svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: repo, homeCredentials: home, httpUpstream: &homeMetadataUpstream{send: send}}
					invoke = func() error { _, err := svc.FetchUpstreamSupportedModels(context.Background(), selected); return err }
				} else {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						resp, err := send(r)
						if err != nil {
							t.Error(err)
							return
						}
						defer resp.Body.Close()
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(resp.StatusCode)
						_, _ = io.Copy(w, resp.Body)
					}))
					defer server.Close()
					originalURL := chatgptCodexModelsURL
					chatgptCodexModelsURL = server.URL
					defer func() { chatgptCodexModelsURL = originalURL }()
					limiter := NewRateLimitService(repo, nil, nil, nil, nil)
					limiter.homeCredentials = home
					svc := &OpenAIGatewayService{accountRepo: repo, homeCredentials: home, rateLimitService: limiter}
					invoke = func() error {
						_, err := svc.FetchCodexModelsManifest(context.Background(), selected, "0.137.0", "")
						return err
					}
				}
				require.Error(t, invoke(), "recovery must preserve the original upstream 401")
				require.Equal(t, int32(1), requests.Load(), "metadata 401 must not cause hidden replay")
				require.Equal(t, "Bearer actual-initial-at", lastAuthorization.Load())
				if scenario == "recover" {
					require.Equal(t, []string{"", "actual-initial-at"}, client.observations())
					require.NoError(t, invoke())
					require.Equal(t, "Bearer recovered-at", lastAuthorization.Load())
				} else {
					require.Equal(t, []string{""}, client.observations(), "old failure must not refresh another UUID or an already-rotated AT")
				}
				if scenario == "rotated" {
					credential, err := home.Resolve(context.Background(), account)
					require.NoError(t, err)
					require.Equal(t, "rotated-at", credential.AccessToken)
				}
				require.Zero(t, client.acquires.Load())
				require.Zero(t, repo.errors.Load(), "Home metadata errors must not permanently disable a new native owner")
			})
		}
	}
}

func TestHomeLiveCapabilityIsUnsupported(t *testing.T) {
	require.False(t, homeTestAccount().SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
}

func TestHomeLiveRejectsManagedCurrentOwner(t *testing.T) {
	for _, kind := range []string{"managed", "stale", "shadow"} {
		t.Run(kind, func(t *testing.T) {
			owner := homeTestAccount()
			repo := &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}
			selected := *owner
			if kind != "managed" {
				selected.Extra = nil
				selected.Credentials = map[string]any{"access_token": "stale-native-at"}
			}
			if kind == "shadow" {
				selected.ID = 8
				selected.ParentAccountID = &owner.ID
				repo.replace(&selected)
			}
			client := &homeMetadataClient{}
			upstream := &liveHTTPUpstreamStub{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo, homeCredentials: NewHomeCredentialsService(repo, client), httpUpstream: upstream}
			_, err := svc.createUpstreamLiveCall(context.Background(), &selected, &LiveCallRequest{SDP: "v=offer\r\n", Session: json.RawMessage(`{"model":"gpt-live-test"}`)}, `{"v":1}`)
			require.ErrorContains(t, err, "Home-managed accounts do not support Live")
			require.Nil(t, upstream.request)
			require.Empty(t, client.observations())
			require.Zero(t, client.acquires.Load())
			_, err = svc.dialLiveSideband(context.Background(), &LiveCallRecord{AccountID: selected.ID, CallID: "call_test"})
			require.ErrorContains(t, err, "Home-managed accounts do not support Live")
		})
	}
}

func TestHomeLiveRejectsConversionAtCredentialBoundaries(t *testing.T) {
	for _, switchAt := range []int32{2, 3} {
		t.Run(map[int32]string{2: "token", 3: "send"}[switchAt], func(t *testing.T) {
			native := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "native-at", "chatgpt_account_id": "native-workspace"}}
			repo := &homeAuxiliarySequenceRepo{before: native, after: homeTestAccount(), switchAt: switchAt}
			client := &homeMetadataClient{}
			upstream := &liveHTTPUpstreamStub{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo, homeCredentials: NewHomeCredentialsService(repo, client), httpUpstream: upstream}
			_, err := svc.createUpstreamLiveCall(context.Background(), native, &LiveCallRequest{SDP: "v=offer\r\n", Session: json.RawMessage(`{"model":"gpt-live-test"}`)}, `{"v":1}`)
			require.ErrorIs(t, err, ErrHomeLiveUnsupported)
			require.Nil(t, upstream.request)
			require.Empty(t, client.observations())
			require.Zero(t, client.acquires.Load())
		})
	}
}

type homeLiveScheduler struct {
	OpenAIAccountScheduler
	account  *Account
	released atomic.Int32
}

func (s *homeLiveScheduler) Select(_ context.Context, request OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	if _, excluded := request.ExcludedIDs[s.account.ID]; excluded {
		return nil, OpenAIAccountScheduleDecision{}, ErrNoAvailableAccounts
	}
	return &AccountSelectionResult{Account: s.account, Acquired: true, ReleaseFunc: func() { s.released.Add(1) }}, OpenAIAccountScheduleDecision{}, nil
}

type homeLiveConcurrency struct {
	liveTestConcurrencyCache
	acquired atomic.Int32
}

func (c *homeLiveConcurrency) AcquireLiveLease(context.Context, int64, int, int64, int, int64, string, bool) (bool, error) {
	c.acquired.Add(1)
	return true, nil
}

func TestHomeLiveStaleSelectionRejectedBeforeSessionLease(t *testing.T) {
	owner := homeTestAccount()
	stale := *owner
	stale.Extra = nil
	repo := &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}
	scheduler := &homeLiveScheduler{account: &stale}
	concurrency := &homeLiveConcurrency{}
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "live-test"}}
	client := &homeMetadataClient{}
	svc := &OpenAIGatewayService{
		cfg: cfg, accountRepo: repo, homeCredentials: NewHomeCredentialsService(repo, client),
		cache: &liveTestStore{GatewayCache: &stubGatewayCache{}}, concurrencyService: NewConcurrencyService(concurrency),
		liveAttestation: liveAttestationStub{header: `{"v":1}`}, liveAttestationCipher: newLiveAttestationCipher(cfg),
		openaiScheduler: scheduler, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
	}
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	_, err := svc.CreateLiveCall(context.Background(), &LiveCallRequest{SDP: "v=offer\r\n", Session: json.RawMessage(`{"model":"gpt-live-test"}`)}, LiveCallIdentity{UserID: 1, APIKeyID: 2}, 1)
	require.ErrorIs(t, err, ErrHomeLiveUnsupported)
	require.Equal(t, int32(1), scheduler.released.Load())
	require.Zero(t, concurrency.acquired.Load())
	require.Zero(t, client.acquires.Load())
	require.Empty(t, client.observations())
}
