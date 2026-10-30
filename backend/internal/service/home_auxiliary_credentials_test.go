package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

// This repository makes ownership changes deterministic at resolution boundaries.
type homeAuxiliarySequenceRepo struct {
	AccountRepository
	before, after *Account
	calls         atomic.Int32
	switchAt      int32
}

func (r *homeAuxiliarySequenceRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.calls.Add(1) >= r.switchAt {
		return r.after, nil
	}
	return r.before, nil
}

type homeAuxiliaryClient struct {
	mu       sync.Mutex
	observed []string
	refresh  func(string, string) (homeclient.Credential, error)
}

func (*homeAuxiliaryClient) Ready() bool { return true }
func (c *homeAuxiliaryClient) Refresh(_ context.Context, id, observed string) (homeclient.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed = append(c.observed, observed)
	if c.refresh != nil {
		return c.refresh(id, observed)
	}
	return homeclient.Credential{ID: id, AccessToken: "token-" + id, AccountID: "account-" + id, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (*homeAuxiliaryClient) Acquire(context.Context, homeclient.Request) (*homeclient.Lease, error) {
	return nil, errors.New("unexpected Acquire")
}
func (c *homeAuxiliaryClient) observations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.observed...)
}

func TestHomePluginConversionCannotExportToken(t *testing.T) {
	for _, switchAt := range []int32{3, 4, 5} {
		t.Run(map[int32]string{3: "provider_initial_read", 4: "provider_final_read", 5: "provider_version_read"}[switchAt], func(t *testing.T) {
			native := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "native", "chatgpt_account_id": "native-account"}}
			repo := &homeAuxiliarySequenceRepo{before: native, after: homeTestAccount(), switchAt: switchAt}
			client := &homeAuxiliaryClient{}
			home := NewHomeCredentialsService(repo, client)
			provider := NewOpenAITokenProvider(repo, &stubQuotaTokenCache{}, nil)
			provider.homeCredentials = home
			gateway := &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider, homeCredentials: home}
			scope := newPluginAccountScope(pluginAccountScopeEntry{Platform: PlatformOpenAI, AccountType: AccountTypeOAuth})
			identity, err := gateway.ResolvePluginOutboundIdentity(context.Background(), scope, 7)
			require.ErrorIs(t, err, ErrHomeOwnershipChanged)
			require.Nil(t, identity, "independent plugins must never receive Home credentials")
			require.Empty(t, client.observations(), "export must reject before asking Home for a credential")
		})
	}
}

func TestHomeCredentialsNativeOnlyBoundaryRejectsConversion(t *testing.T) {
	for _, operation := range []string{"resolve", "prepare", "acquire"} {
		t.Run(operation, func(t *testing.T) {
			native := *homeTestAccount()
			native.Extra = nil
			repo := &homeAuxiliarySequenceRepo{before: &native, after: homeTestAccount(), switchAt: 1}
			client := &homeAuxiliaryClient{}
			home := NewHomeCredentialsService(repo, client)
			ctx := WithNativeCredentialMutation(context.Background())
			var err error
			switch operation {
			case "resolve":
				_, err = home.Resolve(ctx, &native)
			case "prepare":
				_, err = home.Prepare(ctx, &native)
			case "acquire":
				_, err = home.Acquire(ctx, &native, homeclient.Request{})
			}
			require.ErrorIs(t, err, ErrHomeOwnershipChanged)
			require.Empty(t, client.observations())
		})
	}
}

func TestHomeQuotaUsesOneCredentialSnapshot(t *testing.T) {
	first := homeTestAccount()
	second := homeTestAccount()
	second.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
	repo := &homeAuxiliarySequenceRepo{before: first, after: second, switchAt: 5}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.homeCredentials = NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
	quota := NewOpenAIQuotaService(repo, nil, provider, func(string) (*req.Client, error) { return nil, nil }, nil)
	token, identity, _, _, err := quota.prepareUpstreamCall(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, "token-"+testHomeID, token)
	require.Equal(t, "account-"+testHomeID, identity, "token and account identity must come from the same prepared Home result")
}

func TestHomeCredentialsPreparedBindingMatchesResolvedCredential(t *testing.T) {
	first, second := homeTestAccount(), homeTestAccount()
	second.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
	repo := &homeAuxiliarySequenceRepo{before: first, after: second, switchAt: 2}
	home := NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
	snapshot, err := home.Prepare(context.Background(), first)
	require.NoError(t, err)
	id, err := snapshot.HomeCredentialID()
	require.NoError(t, err)
	require.Equal(t, "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e", id)
	require.Equal(t, "token-0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e", snapshot.GetOpenAIAccessToken())
}

func TestHomeCredentialsRecoveryRejectsReboundSnapshot(t *testing.T) {
	for _, switchAt := range []int32{1, 2} {
		t.Run(map[int32]string{1: "initial_read", 2: "refresh_read"}[switchAt], func(t *testing.T) {
			first, second := homeTestAccount(), homeTestAccount()
			second.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
			repo := &homeAuxiliarySequenceRepo{before: first, after: second, switchAt: switchAt}
			client := &homeAuxiliaryClient{}
			home := NewHomeCredentialsService(repo, client)
			require.ErrorIs(t, home.Recover(context.Background(), first, "rejected-first-at"), ErrHomeOwnershipChanged)
			require.Empty(t, client.observations(), "the old credential's 401 must not refresh the new UUID")
		})
	}
}

func TestHomeQuotaLateNativeConversionFailsWithoutSending(t *testing.T) {
	native := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "native", "chatgpt_account_id": "native-account"}}
	repo := &homeAuxiliarySequenceRepo{before: native, after: homeTestAccount(), switchAt: 3}
	client := &homeAuxiliaryClient{}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.homeCredentials = NewHomeCredentialsService(repo, client)
	requests := 0
	quota := NewOpenAIQuotaService(repo, nil, provider, homeAuxiliaryHTTPFactory(func(*http.Request) (int, string) {
		requests++
		return http.StatusOK, `{}`
	}), nil)
	_, err := quota.QueryUsage(context.Background(), 7)
	require.Error(t, err)
	require.Zero(t, requests, "a late conversion must not mix Home AT with native account identity")
	require.Empty(t, client.observations())
}

func TestHomeQuotaPreparedAuthenticationCannotBeOverwritten(t *testing.T) {
	first := homeTestAccount()
	key, privateKey := newTestAgentIdentityKey(t)
	second := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": key.runtimeID,
		"agent_private_key": privateKey, "task_id": key.taskID, "chatgpt_account_id": "agent-workspace",
	}}
	repo := &homeAuxiliarySequenceRepo{before: first, after: second, switchAt: 5}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.homeCredentials = NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
	var sent []string
	quota := NewOpenAIQuotaService(repo, nil, provider, homeAuxiliaryHTTPFactory(func(r *http.Request) (int, string) {
		sent = append(sent, r.Header.Get("Authorization"))
		require.Equal(t, "account-"+testHomeID, r.Header.Get("Chatgpt-Account-Id"))
		return http.StatusOK, `{}`
	}), nil)
	_, err := quota.QueryUsage(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer token-" + testHomeID, "Bearer token-" + testHomeID}, sent, "late account reads must not replace just one half of the prepared authentication")
}

func homeAuxiliaryHTTPFactory(send func(*http.Request) (int, string)) PrivacyClientFactory {
	client := req.C()
	client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			status, body := send(r)
			return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
		}
	})
	return func(string) (*req.Client, error) { return client, nil }
}

func TestHomeQuota401RecoversObservedTokenWithoutReplay(t *testing.T) {
	for _, path := range []string{"usage", "details", "consume", "targeted_consume"} {
		t.Run(path, func(t *testing.T) {
			account := homeTestAccount()
			repo := &homeAuxiliarySequenceRepo{before: account, after: account, switchAt: 1}
			client := &homeAuxiliaryClient{refresh: func(id, observed string) (homeclient.Credential, error) {
				token := "rejected-at"
				if observed != "" {
					token = "recovered-at"
				}
				return homeclient.Credential{ID: id, AccessToken: token, AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}}
			provider := NewOpenAITokenProvider(repo, nil, nil)
			provider.homeCredentials = NewHomeCredentialsService(repo, client)
			var sent []string
			firstCall := true
			factory := homeAuxiliaryHTTPFactory(func(r *http.Request) (int, string) {
				sent = append(sent, r.Header.Get("Authorization"))
				require.Equal(t, "workspace", r.Header.Get("Chatgpt-Account-Id"))
				if firstCall && (path != "details" || strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits")) {
					return http.StatusUnauthorized, `{"error":"unauthorized"}`
				}
				return http.StatusOK, `{}`
			})
			quota := NewOpenAIQuotaService(repo, nil, provider, factory, nil)
			invoke := func() error {
				switch path {
				case "consume":
					_, err := quota.ResetCredit(context.Background(), 7)
					return err
				case "targeted_consume":
					_, err := quota.ResetCreditTargeted(context.Background(), 7, "credit", "redeem")
					return err
				default:
					_, err := quota.QueryUsage(context.Background(), 7)
					return err
				}
			}
			err := invoke()
			if path == "details" {
				require.NoError(t, err, "optional details failure must preserve successful usage response")
				require.Equal(t, []string{"Bearer rejected-at", "Bearer rejected-at"}, sent)
			} else {
				require.ErrorContains(t, err, "401", "recovery must preserve the original upstream failure")
				require.Equal(t, []string{"Bearer rejected-at"}, sent, "Home recovery must not replay the upstream operation")
			}
			require.Equal(t, []string{"", "rejected-at"}, client.observations())
			firstCall = false
			previousRequests := len(sent)
			require.NoError(t, invoke())
			for _, token := range sent[previousRequests:] {
				require.Equal(t, "Bearer recovered-at", token)
			}
			require.Len(t, client.observations(), 2, "next explicit call must use recovered cache without rotation")
		})
	}
}

func TestHomeQuota401RecoveryFailureDoesNotReuseRejectedCache(t *testing.T) {
	account := homeTestAccount()
	repo := &homeAuxiliarySequenceRepo{before: account, after: account, switchAt: 1}
	client := &homeAuxiliaryClient{refresh: func(id, observed string) (homeclient.Credential, error) {
		if observed != "" {
			return homeclient.Credential{}, errors.New("offline")
		}
		return homeclient.Credential{ID: id, AccessToken: "rejected-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.homeCredentials = NewHomeCredentialsService(repo, client)
	requests := 0
	quota := NewOpenAIQuotaService(repo, nil, provider, homeAuxiliaryHTTPFactory(func(*http.Request) (int, string) {
		requests++
		return http.StatusUnauthorized, `{}`
	}), nil)
	_, err := quota.QueryUsage(context.Background(), 7)
	require.ErrorContains(t, err, "401")
	for range 5 {
		_, err = quota.QueryUsage(context.Background(), 7)
		require.Error(t, err)
	}
	require.Equal(t, 1, requests, "failed recovery must remove the rejected token")
	require.Equal(t, []string{"", "rejected-at"}, client.observations(), "recovery errors must be bounded")
}

func TestHomeQuota401CooldownInvalidatesNewlyRejectedToken(t *testing.T) {
	account := homeTestAccount()
	repo := &homeAuxiliarySequenceRepo{before: account, after: account, switchAt: 1}
	client := &homeAuxiliaryClient{refresh: func(id, observed string) (homeclient.Credential, error) {
		token := "first-at"
		if observed != "" {
			token = "second-at"
		}
		return homeclient.Credential{ID: id, AccessToken: token, AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.homeCredentials = NewHomeCredentialsService(repo, client)
	var tokens []string
	quota := NewOpenAIQuotaService(repo, nil, provider, homeAuxiliaryHTTPFactory(func(r *http.Request) (int, string) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		return http.StatusUnauthorized, `{}`
	}), nil)
	for range 5 {
		_, err := quota.QueryUsage(context.Background(), 7)
		require.Error(t, err)
	}
	require.Equal(t, []string{"Bearer first-at", "Bearer second-at"}, tokens, "cooldown must block reuse once the recovered token is also rejected")
	require.Equal(t, []string{"", "first-at"}, client.observations(), "401 burst must remain bounded")
}

func TestHomeCredentialsConcurrentRecoveryKeepsRecoveredToken(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	client := &homeAuxiliaryClient{refresh: func(id, observed string) (homeclient.Credential, error) {
		token := "first-at"
		if observed != "" {
			close(started)
			<-release
			token = "second-at"
		}
		return homeclient.Credential{ID: id, AccessToken: token, AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	home := NewHomeCredentialsService(nil, client)
	account := homeTestAccount()
	_, err := home.Resolve(context.Background(), account)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- home.Recover(context.Background(), account, "first-at") }()
	<-started
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := home.Recover(context.Background(), account, "first-at"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(release)
	require.NoError(t, <-done)
	// A delayed 401 for the old token must not invalidate its replacement.
	require.NoError(t, home.Recover(context.Background(), account, "first-at"))
	credential, err := home.Resolve(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "second-at", credential.AccessToken)
	require.Equal(t, []string{"", "first-at"}, client.observations())
}

func TestHomeCredentialsOfflineRecoveryInvalidatesRejectedToken(t *testing.T) {
	client := &homeClientFake{ready: true, credential: homeclient.Credential{ID: testHomeID, AccessToken: "rejected-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, client)
	account := homeTestAccount()
	_, err := home.Resolve(context.Background(), account)
	require.NoError(t, err)
	client.ready = false
	require.ErrorIs(t, home.Recover(context.Background(), account, "rejected-at"), ErrHomeUnavailable)
	client.ready = true
	client.credential.AccessToken = "reconnected-at"
	credential, err := home.Resolve(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "reconnected-at", credential.AccessToken, "Home reconnect must not revive the rejected warm cache entry")
}
