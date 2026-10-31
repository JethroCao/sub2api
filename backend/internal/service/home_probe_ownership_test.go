package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The SSE start event is emitted after credentials are prepared but before the
// request is dispatched, so this exercises the real probe ownership boundary.
type homeProbeWriter struct {
	*httptest.ResponseRecorder
	onStart func()
	once    sync.Once
}

func (w *homeProbeWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "test_start") {
		w.once.Do(w.onStart)
	}
	return w.ResponseRecorder.Write(p)
}

type homeProbeIdentityCache struct {
	IdentityCache
	onRead func()
}

type homeProbeRepo struct {
	*homeMetadataRepo
	onUpdate func(context.Context, int64)
	onGet    func(*Account) *Account
}

func (r *homeProbeRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	a, err := r.homeMetadataRepo.GetByID(ctx, id)
	if err == nil && r.onGet != nil {
		a = r.onGet(a)
	}
	return a, err
}

func (r *homeProbeRepo) UpdateExtra(ctx context.Context, id int64, _ map[string]any) error {
	if r.onUpdate != nil {
		r.onUpdate(ctx, id)
	}
	return nil
}

func (c *homeProbeIdentityCache) GetFingerprint(context.Context, int64) (*Fingerprint, error) {
	c.onRead()
	return nil, nil
}

func homeProbeAccounts(shadow bool, scenario string) (*Account, *Account, *homeProbeRepo, func()) {
	owner := homeTransportAccount()
	if scenario == "native" || scenario == "native_to_home" {
		owner.Extra = nil
		owner.Credentials = map[string]any{"access_token": "native-token", "chatgpt_account_id": "native-account"}
	}
	selected := owner
	if shadow {
		selected = &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID, Concurrency: 3, Credentials: owner.Credentials}
	}
	repo := &homeProbeRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{owner.ID: owner, selected.ID: selected}}}
	change := func() {
		next := *owner
		switch scenario {
		case "native_to_home":
			next.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: transportHomeTestID}
		case "unbind":
			next.Extra = nil
			next.Credentials = map[string]any{"access_token": "new-native-token", "chatgpt_account_id": "new-native-account"}
		case "rebind", "during_admission":
			next.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: homeRecoveryOldID}
		default:
			return
		}
		if scenario == "during_admission" {
			reads := 0
			repo.onGet = func(a *Account) *Account {
				if a.ID == owner.ID {
					reads++
					if reads > 2 {
						return &next
					}
				}
				return a
			}
		} else {
			repo.replace(&next)
		}
	}
	// Repository reads return persisted rows independently from the caller's
	// mutable probe snapshot (compact probes merge capability hints locally).
	snapshot := *selected
	snapshot.Extra = shallowCopyMap(selected.Extra)
	snapshot.Credentials = shallowCopyMap(selected.Credentials)
	return owner, &snapshot, repo, change
}

func TestHomeAccountTest401DoesNotDisableAccount(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, shadow := range []bool{false, true} {
			t.Run(map[bool]string{false: "responses", true: "compact"}[compact]+"/"+map[bool]string{false: "direct", true: "shadow"}[shadow], func(t *testing.T) {
				h, gateway := homeTransportSetup(t)
				_, selected, repo, _ := homeProbeAccounts(shadow, "stable")
				gateway.homeCredentials.accounts = repo
				upstream := &homeMetadataUpstream{send: func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_revoked"}}`))}, nil
				}}
				svc := &AccountTestService{accountRepo: repo, homeCredentials: gateway.homeCredentials, httpUpstream: upstream}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
				mode := ""
				if compact {
					mode = AccountTestModeCompact
				}
				require.Error(t, svc.testOpenAIAccountConnection(c, selected, transportHomeTestModel, "test", mode))
				require.Zero(t, repo.errors.Load(), "the transport already owns exact-token Home 401 recovery; a probe must not permanently disable it")
				require.Equal(t, 1, h.count("RPOP", ""))
			})
		}
	}
}

func TestHomeAccountTestNativeConversionDoesNotDisableAccount(t *testing.T) {
	h, gateway := homeTransportSetup(t)
	_, selected, repo, change := homeProbeAccounts(false, "native_to_home")
	gateway.homeCredentials.accounts = repo
	upstream := &homeMetadataUpstream{send: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_revoked"}}`))}, nil
	}}
	svc := &AccountTestService{accountRepo: repo, homeCredentials: gateway.homeCredentials, httpUpstream: upstream}
	c, _ := gin.CreateTestContext(&homeProbeWriter{ResponseRecorder: httptest.NewRecorder(), onStart: change})
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	require.Error(t, svc.testOpenAIAccountConnection(c, selected, transportHomeTestModel, "test", ""))
	require.Zero(t, repo.errors.Load(), "native-prepared probes must not treat a newly adopted Home 401 as a native credential failure")
	require.Zero(t, h.count("RPOP", ""))
}

type homeProbeRoundTripper func(*http.Request) (*http.Response, error)

func (f homeProbeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHomeUsageProbeSnapshotPreservesResultOwnership(t *testing.T) {
	h, gateway := homeTransportSetup(t)
	owner, selected, repo, change := homeProbeAccounts(true, "unbind")
	gateway.homeCredentials.accounts = repo
	// Isolate the shared HTTP client by a unique local proxy key, then replace
	// its transport so this real probe cannot contact a production endpoint.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected proxy connection") }))
	defer proxy.Close()
	u, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	proxyID := int64(9)
	selected.ProxyID = &proxyID
	selected.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: u.Hostname(), Port: port}
	client, err := httppool.GetClient(httppool.Options{ProxyURL: selected.Proxy.URL(), Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
	require.NoError(t, err)
	originalTransport := client.Transport
	defer func() { client.Transport = originalTransport }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.Transport = homeProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
		change()
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Codex-Primary-Used-Percent": []string{"42"}, "X-Codex-Primary-Window-Minutes": []string{"300"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	updated := make(chan context.Context, 1)
	repo.onUpdate = func(ctx context.Context, id int64) {
		require.Equal(t, selected.ID, id)
		if ctx.Err() != nil {
			t.Error("request cancellation must not cancel detached snapshot persistence")
		}
		updated <- ctx
	}
	svc := &AccountUsageService{accountRepo: repo, homeCredentials: gateway.homeCredentials}
	_, err = svc.probeOpenAICodexSnapshot(ctx, selected)
	require.NoError(t, err)
	select {
	case updateCtx := <-updated:
		id, ok := HomeResultCredentialID(updateCtx)
		require.True(t, ok, "a detached probe snapshot must keep the original Home result owner for the repository CAS")
		require.Equal(t, transportHomeTestID, id)
		ownerID, ok := HomeResultOwnerID(updateCtx)
		require.True(t, ok)
		require.Equal(t, owner.ID, ownerID)
		selectedID, ok := HomeResultAccountID(updateCtx)
		require.True(t, ok)
		require.Equal(t, selected.ID, selectedID)
		marker, ok := updateCtx.Value(homeRequestCredentialKey{}).(homeRequestCredential)
		require.True(t, ok)
		require.Equal(t, owner.ID, marker.ownerID)
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not persist its snapshot")
	}
	require.Equal(t, 1, h.count("RPOP", ""))
}

func TestHomeAccountTestPreparedOwnership(t *testing.T) {
	for _, mode := range []string{"responses", "compact", "images_direct", "images_responses"} {
		for _, shadow := range []bool{false, true} {
			for _, scenario := range []string{"unbind", "rebind", "during_admission", "native_to_home", "stable", "native"} {
				t.Run(mode+"/"+map[bool]string{false: "direct", true: "shadow"}[shadow]+"/"+scenario, func(t *testing.T) {
					h, gateway := homeTransportSetup(t)
					owner, selected, repo, change := homeProbeAccounts(shadow, scenario)
					gateway.homeCredentials.accounts = repo
					if scenario == "during_admission" {
						h.mu.Lock()
						h.mutate = func(reply map[string]any) {
							reply["auth_index"] = homeRecoveryOldID
							reply["auth"].(map[string]any)["id"] = homeRecoveryOldID
							reply["concurrency"].(map[string]any)["credential_id"] = homeRecoveryOldID
						}
						h.mu.Unlock()
					}
					var sends int
					upstream := &homeMetadataUpstream{send: func(req *http.Request) (*http.Response, error) {
						sends++
						if scenario == "native" {
							require.Equal(t, "Bearer native-token", req.Header.Get("Authorization"))
						} else if scenario == "stable" {
							require.Equal(t, "Bearer access-token", req.Header.Get("Authorization"))
							marker, ok := req.Context().Value(homeRequestCredentialKey{}).(homeRequestCredential)
							require.True(t, ok)
							require.Equal(t, selected.ID, marker.accountID)
							require.Equal(t, owner.ID, marker.ownerID)
							require.Equal(t, transportHomeTestID, marker.credential.ID)
						}
						return nil, errors.New("synthetic probe transport stop")
					}}
					svc := &AccountTestService{accountRepo: repo, homeCredentials: gateway.homeCredentials, httpUpstream: upstream}
					writer := &homeProbeWriter{ResponseRecorder: httptest.NewRecorder(), onStart: change}
					c, _ := gin.CreateTestContext(writer)
					c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
					var err error
					switch mode {
					case "responses":
						err = svc.testOpenAIAccountConnection(c, selected, transportHomeTestModel, "test", "")
					case "compact":
						err = svc.testOpenAIAccountConnection(c, selected, transportHomeTestModel, "test", AccountTestModeCompact)
					case "images_direct":
						err = svc.testOpenAIAccountConnection(c, selected, "gpt-image-2", "test", "")
					case "images_responses":
						err = svc.testOpenAIAccountConnection(c, selected, "gpt-image-1", "test", "")
					}
					require.Error(t, err)
					if scenario == "during_admission" {
						require.Zero(t, sends)
						require.Equal(t, 1, h.count("RPOP", ""))
						transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 && h.count("LPUSH", "usage") == 1 })
						h.mu.Lock()
						for _, record := range h.records {
							if record.args[0] == "LPUSH" && record.args[1] == "usage" {
								require.Contains(t, record.args[2], `"status_code":499`)
							}
						}
						h.mu.Unlock()
					} else if scenario == "unbind" || scenario == "rebind" || scenario == "native_to_home" {
						require.Zero(t, sends, "a prepared Home probe must stop before upstream dispatch after ownership changes")
						require.Zero(t, h.count("RPOP", ""), "changed prepared identity must be rejected before admission")
					} else {
						require.Equal(t, 1, sends)
						require.Equal(t, map[bool]int{false: 0, true: 1}[scenario == "stable"], h.count("RPOP", ""))
					}
				})
			}
		}
	}
}

func TestHomeUsageProbePreparedOwnership(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		for _, scenario := range []string{"unbind", "rebind", "native_to_home", "stable", "native"} {
			t.Run(map[bool]string{false: "direct", true: "shadow"}[shadow]+"/"+scenario, func(t *testing.T) {
				h, gateway := homeTransportSetup(t)
				_, selected, repo, change := homeProbeAccounts(shadow, scenario)
				gateway.homeCredentials.accounts = repo
				var connects atomic.Int32
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					connects.Add(1)
					w.WriteHeader(http.StatusBadGateway)
				}))
				defer proxy.Close()
				u, err := url.Parse(proxy.URL)
				require.NoError(t, err)
				port, err := strconv.Atoi(u.Port())
				require.NoError(t, err)
				proxyID := int64(9)
				selected.ProxyID = &proxyID
				selected.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: u.Hostname(), Port: port}
				svc := &AccountUsageService{accountRepo: repo, homeCredentials: gateway.homeCredentials, identityCache: &homeProbeIdentityCache{onRead: change}}
				_, err = svc.probeOpenAICodexSnapshot(t.Context(), selected)
				require.Error(t, err)
				if scenario == "unbind" || scenario == "rebind" || scenario == "native_to_home" {
					require.Zero(t, connects.Load(), "usage probe must reject changed ownership before opening any upstream connection")
					require.Zero(t, h.count("RPOP", ""))
				} else {
					require.EqualValues(t, 1, connects.Load())
					require.Equal(t, map[bool]int{false: 0, true: 1}[scenario == "stable"], h.count("RPOP", ""))
				}
			})
		}
	}
}
