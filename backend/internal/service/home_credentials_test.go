package service

import (
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/stretchr/testify/require"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type homeClientFake struct {
	calls      atomic.Int32
	ready      bool
	err        error
	credential homeclient.Credential
}

func (f *homeClientFake) Ready() bool { return f.ready }
func (f *homeClientFake) Refresh(context.Context, string, string) (homeclient.Credential, error) {
	f.calls.Add(1)
	return f.credential, f.err
}
func (f *homeClientFake) Acquire(context.Context, homeclient.Request) (*homeclient.Lease, error) {
	return nil, errors.New("unused")
}

type homeNativeCacheTrap struct {
	OpenAITokenCache
	calls int
}

func (c *homeNativeCacheTrap) GetAccessToken(context.Context, string) (string, error) {
	c.calls++
	return "poison", nil
}
func homeTestAccount() *Account {
	return &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: homeTestExtra(), Credentials: map[string]any{"refresh_token": "poison"}}
}
func TestHomeCredentialsCoalesceCacheAndCopyIdentity(t *testing.T) {
	a := homeTestAccount()
	client := &homeClientFake{ready: true, credential: homeclient.Credential{ID: testHomeID, AccessToken: "home-at", AccountID: "home-account", ExpiresAt: time.Now().Add(time.Hour)}}
	s := NewHomeCredentialsService(nil, client)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Resolve(context.Background(), a)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), client.calls.Load())
	copy, err := s.Prepare(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, a.ID, copy.ID)
	require.Equal(t, "home-at", copy.GetOpenAIAccessToken())
	require.Equal(t, "home-account", copy.GetCredential("chatgpt_account_id"))
	require.Equal(t, "poison", a.GetOpenAIRefreshToken())
	require.Empty(t, copy.GetOpenAIRefreshToken())
}
func TestHomeCredentialsStaleNativeSnapshotNeverUsesNativeCache(t *testing.T) {
	current := homeTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{7: current}}
	cache := &homeNativeCacheTrap{}
	p := NewOpenAITokenProvider(repo, cache, nil)
	stale := *current
	stale.Extra = nil
	_, err := p.GetAccessToken(context.Background(), &stale)
	require.ErrorIs(t, err, ErrHomeUnavailable)
	require.Zero(t, cache.calls)
}
func TestHomeCredentialsRepositoryOwnershipBoundary(t *testing.T) {
	a := homeTestAccount()
	a.Extra = nil
	require.ErrorIs(t, ProtectHomeAccountWrite(context.Background(), homeTestExtra(), a), ErrHomeOwnershipChanged)
	a = homeTestAccount()
	require.NoError(t, ProtectHomeAccountWrite(context.Background(), homeTestExtra(), a))
	require.Empty(t, a.GetOpenAIRefreshToken())
	a = homeTestAccount()
	a.Extra = nil
	ctx, err := prepareHomeBindingUpdate(context.Background(), a, homeTestExtra())
	require.NoError(t, err)
	a.Extra = homeTestExtra()
	require.NoError(t, ProtectHomeAccountWrite(ctx, nil, a))
	require.ErrorIs(t, ProtectHomeAccountWrite(ctx, homeTestExtra(), a), ErrHomeOwnershipChanged)
}
func TestHomeCredentials401DoesNotPermanentlyDisable(t *testing.T) {
	a := homeTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{7: a}}
	s := NewRateLimitService(repo, nil, nil, nil, nil)
	require.False(t, s.HandleUpstreamError(context.Background(), a, http.StatusUnauthorized, nil, []byte(`{"error":{"code":"token_revoked"}}`)))
	require.Equal(t, StatusActive, repo.accounts[7].Status)
}

func TestHomeCredentialsExpiredResponseRejected(t *testing.T) {
	client := &homeClientFake{ready: true, credential: homeclient.Credential{ID: testHomeID, AccessToken: "expired", AccountID: "home-account", ExpiresAt: time.Now().Add(-time.Minute)}}
	_, err := NewHomeCredentialsService(nil, client).Resolve(context.Background(), homeTestAccount())
	require.Error(t, err)
}
func TestHomeCredentialsFailedRecoveryBackoff(t *testing.T) {
	client := &homeClientFake{ready: true, err: errors.New("offline")}
	s := NewHomeCredentialsService(nil, client)
	for range 10 {
		_, err := s.Refresh(context.Background(), homeTestAccount(), "rejected-at")
		require.Error(t, err)
	}
	require.Equal(t, int32(1), client.calls.Load())
}
func TestHomeCredentialsNativeRefreshRejectsStaleSnapshot(t *testing.T) {
	current := homeTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{7: current}}
	stale := *current
	stale.Extra = nil
	r := NewOpenAITokenRefresher(nil, repo)
	_, err := r.Refresh(context.Background(), &stale)
	require.ErrorIs(t, err, ErrHomeOwnershipChanged)
}

func TestHomeCredentials401WithoutObservedTokenRefreshesOnce(t *testing.T) {
	a := homeTestAccount()
	a.Credentials = nil
	client := &homeClientFake{ready: true, credential: homeclient.Credential{ID: testHomeID, AccessToken: "token", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, client)
	_, err := home.Resolve(context.Background(), a)
	require.NoError(t, err)
	limiter := &RateLimitService{homeCredentials: home}
	for range 10 {
		require.False(t, limiter.HandleUpstreamError(context.Background(), a, http.StatusUnauthorized, nil, nil))
	}
	require.Equal(t, int32(2), client.calls.Load())
}

func TestHomeCredentialsSnapshotPreservesSelectedAccountModelMappings(t *testing.T) {
	owner := homeTestAccount()
	selected := *owner
	selected.ID = 8
	selected.Credentials = map[string]any{"model_mapping": map[string]any{"local": "mapped"}, "refresh_token": "stale"}
	snapshot := ApplyHomeCredential(&selected, owner, homeclient.Credential{AccessToken: "home", AccountID: "chatgpt"})
	require.Equal(t, int64(8), snapshot.ID)
	require.Equal(t, selected.Credentials["model_mapping"], snapshot.Credentials["model_mapping"])
	require.Empty(t, snapshot.GetOpenAIRefreshToken())
}
