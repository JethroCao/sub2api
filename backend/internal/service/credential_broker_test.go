//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

const brokerFixtureKey = "fixture-node-secret-with-at-least-32-bytes"

type brokerTestRepo struct {
	AccountRepository
	mu         sync.Mutex
	a          *Account
	reads      int
	cas        func()
	writes     int
	casErr     error
	queriedIDs []int64
}

func (r *brokerTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	r.queriedIDs = append(r.queriedIDs, id)
	if r.a == nil || r.a.ID != id {
		return nil, ErrAccountNotFound
	}
	a := *r.a
	a.Credentials = shallowCopyMap(a.Credentials)
	a.Extra = shallowCopyMap(a.Extra)
	return &a, nil
}
func (r *brokerTestRepo) UpdateOpenAIBrokerCredentialsIfUnchanged(_ context.Context, id int64, expected, updated map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cas != nil {
		r.cas()
	}
	if r.casErr != nil {
		return false, r.casErr
	}
	if r.a.ID != id || r.a.HasHomeBinding() || !reflect.DeepEqual(CredentialBrokerAuthSnapshot(r.a.Credentials), CredentialBrokerAuthSnapshot(expected)) {
		return false, nil
	}
	for k, v := range CredentialBrokerAuthUpdate(updated) {
		r.a.Credentials[k] = v
	}
	r.writes++
	return true, nil
}

type brokerTestCache struct {
	GeminiTokenCache
	mu               sync.Mutex
	held             bool
	lockErr          error
	deleteErr        error
	deleted          int
	deleteCanceled   bool
	deletedUnderLock bool
}

func (c *brokerTestCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lockErr != nil {
		return false, c.lockErr
	}
	if c.held {
		return false, nil
	}
	c.held = true
	return true, nil
}
func (c *brokerTestCache) ReleaseRefreshLock(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = false
	return nil
}
func (c *brokerTestCache) DeleteAccessToken(ctx context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted++
	c.deleteCanceled = ctx.Err() != nil
	c.deletedUnderLock = c.held
	return c.deleteErr
}

func (c *brokerTestCache) AcquireOwnedRefreshLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	ok, err := c.AcquireRefreshLock(ctx, key, ttl)
	return "fixture-owner", ok, err
}
func (c *brokerTestCache) ReleaseOwnedRefreshLock(ctx context.Context, key, owner string) error {
	return c.ReleaseRefreshLock(ctx, key)
}
func (c *brokerTestCache) ExtendOwnedRefreshLock(context.Context, string, string, time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held, nil
}

type brokerTestExecutor struct {
	calls  int
	before func()
	err    error
}

func (e *brokerTestExecutor) CacheKey(a *Account) string                    { return OpenAITokenCacheKey(a) }
func (e *brokerTestExecutor) CanRefresh(a *Account) bool                    { return true }
func (e *brokerTestExecutor) NeedsRefresh(a *Account, _ time.Duration) bool { return false }
func (e *brokerTestExecutor) Refresh(_ context.Context, a *Account) (map[string]any, error) {
	e.calls++
	if e.before != nil {
		e.before()
	}
	if e.err != nil {
		return nil, e.err
	}
	c := shallowCopyMap(a.Credentials)
	c["access_token"] = "rotated-access"
	c["refresh_token"] = "rotated-refresh"
	c["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return c, nil
}

type brokerTestLimitEntry struct {
	n   int
	end time.Time
}
type brokerTestLimiter struct {
	mu      sync.Mutex
	now     time.Time
	closed  bool
	entries map[string]brokerTestLimitEntry
}

func (l *brokerTestLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false, 0, errors.New("limiter down")
	}
	e := l.entries[key]
	if !l.now.Before(e.end) {
		e = brokerTestLimitEntry{end: l.now.Add(window)}
	}
	e.n++
	l.entries[key] = e
	return e.n <= limit, e.end.Sub(l.now), nil
}
func (l *brokerTestLimiter) FastForward(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = l.now.Add(d)
}
func (l *brokerTestLimiter) Close() { l.mu.Lock(); defer l.mu.Unlock(); l.closed = true }

func brokerFixture(t *testing.T) (*CredentialBrokerService, *brokerTestRepo, *brokerTestCache, *brokerTestExecutor, *brokerTestLimiter) {
	t.Helper()
	m := &brokerTestLimiter{now: time.Now(), entries: map[string]brokerTestLimitEntry{}}
	repo := &brokerTestRepo{a: &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "fixture-access", "refresh_token": "NEVER-EXPORT-REFRESH", "id_token": "NEVER-EXPORT-ID", "chatgpt_account_id": "upstream-account", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339), "model_mapping": map[string]any{"alias": "model"}}}}
	cache := &brokerTestCache{}
	executor := &brokerTestExecutor{}
	digest := sha256.Sum256([]byte(brokerFixtureKey))
	cfg := &config.Config{CredentialBroker: config.CredentialBrokerConfig{Enabled: true, Clients: []config.CredentialBrokerClientConfig{{ID: "node-107", KeySHA256: hex.EncodeToString(digest[:]), AccountIDs: []int64{4}}}}}
	svc := NewCredentialBrokerService(repo, NewOAuthRefreshAPI(repo, cache), nil, cache, m, cfg)
	svc.executor = executor
	return svc, repo, cache, executor, m
}
func brokerErrorCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var e *CredentialBrokerError
	require.ErrorAs(t, err, &e)
	require.Equal(t, status, e.Status)
	require.Equal(t, code, e.Code)
}

func TestCredentialBrokerAuthorizationAndProjection(t *testing.T) {
	s, r, _, e, _ := brokerFixture(t)
	ctx := context.Background()
	_, err := s.Issue(ctx, "wrong", 4, "")
	brokerErrorCode(t, err, 401, "unauthorized")
	require.Zero(t, r.reads)
	_, err = s.Issue(ctx, brokerFixtureKey, 14, "")
	brokerErrorCode(t, err, 404, "not_found")
	require.Zero(t, r.reads)
	got, err := s.Issue(ctx, brokerFixtureKey, 4, "")
	require.NoError(t, err)
	require.Equal(t, "fixture-access", got.AccessToken)
	require.Equal(t, "upstream-account", got.ChatGPTAccountID)
	require.Len(t, got.Revision, 64)
	require.Zero(t, e.calls)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	for _, secret := range []string{"refresh_token", "id_token", "NEVER-EXPORT", "model_mapping"} {
		require.NotContains(t, string(data), secret)
	}
	s.enabled = false
	_, err = s.Issue(ctx, brokerFixtureKey, 4, "")
	brokerErrorCode(t, err, 404, "not_found")
}
func TestCredentialBrokerRejectsIneligibleAccounts(t *testing.T) {
	for name, change := range map[string]func(*Account){"inactive": func(a *Account) { a.Status = "inactive" }, "unschedulable": func(a *Account) { a.Schedulable = false }, "shadow": func(a *Account) { n := int64(1); a.ParentAccountID = &n }, "home": func(a *Account) {
		a.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "eb1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
	}, "PAT": func(a *Account) { a.Credentials["auth_mode"] = "pat" }, "api-key": func(a *Account) { a.Type = AccountTypeAPIKey }, "other-platform": func(a *Account) { a.Platform = PlatformAnthropic }, "missing-RT": func(a *Account) { delete(a.Credentials, "refresh_token") }, "expired-account": func(a *Account) { v := time.Now().Add(-time.Hour); a.ExpiresAt = &v; a.AutoPauseOnExpired = true }} {
		t.Run(name, func(t *testing.T) {
			s, r, _, e, _ := brokerFixture(t)
			change(r.a)
			_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
			brokerErrorCode(t, err, 503, "credential_unavailable")
			require.Zero(t, e.calls)
		})
	}
}
func TestCredentialBrokerRefreshAndRevisionDeduplication(t *testing.T) {
	s, r, c, e, m := brokerFixture(t)
	ctx := context.Background()
	first, err := s.Issue(ctx, brokerFixtureKey, 4, "")
	require.NoError(t, err)
	_, err = s.Issue(ctx, brokerFixtureKey, 4, strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Zero(t, e.calls)
	deletionsBeforeRefresh := c.deleted
	next, err := s.Issue(ctx, brokerFixtureKey, 4, first.Revision)
	require.NoError(t, err)
	require.Equal(t, "rotated-access", next.AccessToken)
	require.NotEqual(t, first.Revision, next.Revision)
	require.Equal(t, 1, e.calls)
	require.Equal(t, deletionsBeforeRefresh+2, c.deleted, "invalidate at commit and heal on issuance")
	_, err = s.Issue(ctx, brokerFixtureKey, 4, first.Revision)
	require.NoError(t, err)
	require.Equal(t, 1, e.calls)
	_, err = s.Issue(ctx, brokerFixtureKey, 4, next.Revision)
	brokerErrorCode(t, err, 429, "refresh_throttled")
	require.Equal(t, 1, e.calls)
	m.FastForward(31 * time.Second)
	_, err = s.Issue(ctx, brokerFixtureKey, 4, next.Revision)
	require.NoError(t, err)
	require.Equal(t, 2, e.calls)
	require.Equal(t, map[string]any{"alias": "model"}, r.a.Credentials["model_mapping"])
}
func TestCredentialBrokerExpiredTokenRefreshes(t *testing.T) {
	s, r, _, e, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	got, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.NoError(t, err)
	require.Equal(t, "rotated-access", got.AccessToken)
	require.Equal(t, 1, e.calls)
}
func TestCredentialBrokerFailsClosedOnDependencies(t *testing.T) {
	t.Run("limiter", func(t *testing.T) {
		s, r, _, e, m := brokerFixture(t)
		m.Close()
		_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
		brokerErrorCode(t, err, 503, "broker_unavailable")
		require.Zero(t, r.reads)
		require.Zero(t, e.calls)
	})
	t.Run("refresh-lock", func(t *testing.T) {
		s, r, c, e, _ := brokerFixture(t)
		r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
		c.lockErr = errors.New("redis down")
		_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
		require.Error(t, err)
		require.Zero(t, e.calls)
	})
	t.Run("cache-invalidation", func(t *testing.T) {
		s, r, c, _, _ := brokerFixture(t)
		r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
		c.deleteErr = errors.New("cache down")
		_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
		brokerErrorCode(t, err, 503, "broker_unavailable")
	})
}
func TestCredentialBrokerDoesNotOverwriteReauthorization(t *testing.T) {
	s, r, _, e, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	e.before = func() {
		r.a.Credentials["refresh_token"] = "reauthorized-refresh"
		r.a.Credentials["access_token"] = "reauthorized-access"
	}
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.Error(t, err)
	require.Equal(t, "reauthorized-refresh", r.a.Credentials["refresh_token"])
	require.Zero(t, r.writes)
}
func TestCredentialBrokerRequestLimit(t *testing.T) {
	s, _, _, _, m := brokerFixture(t)
	for i := 0; i < 60; i++ {
		_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
		require.NoError(t, err)
	}
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	brokerErrorCode(t, err, 429, "rate_limited")
	m.FastForward(time.Minute)
	_, err = s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.NoError(t, err)
}
func TestCredentialBrokerRedactsRefreshErrors(t *testing.T) {
	s, r, _, e, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	e.err = errors.New("NEVER-EXPORT-REFRESH fixture-access")
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "NEVER-EXPORT")
	require.NotContains(t, err.Error(), "fixture-access")
}

func TestCredentialBrokerOwnerRereadRejectsAuthChange(t *testing.T) {
	_, r, _, _, _ := brokerFixture(t)
	a, err := r.GetByID(context.Background(), 4)
	require.NoError(t, err)
	ctx := withCredentialBrokerSnapshot(withCredentialBrokerRefresh(WithNativeCredentialMutation(context.Background())), a)
	r.a.Credentials["refresh_token"] = "reauthorized-refresh"
	_, err = currentHomeCredentialOwner(ctx, r, a)
	require.ErrorIs(t, err, ErrCredentialBrokerStateChanged)
}

func TestCredentialBrokerFinalEligibilityAndMetadata(t *testing.T) {
	for name, edit := range map[string]func(*Account){
		"disabled": func(a *Account) { a.Schedulable = false },
		"rebound":  func(a *Account) { a.Extra = map[string]any{HomeCredentialIDKey: "different-owner"} },
		"metadata": func(a *Account) { a.Credentials["model_mapping"] = map[string]any{"new": "preserved"} },
	} {
		t.Run(name, func(t *testing.T) {
			s, r, _, e, _ := brokerFixture(t)
			r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
			e.before = func() { edit(r.a) }
			got, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
			if name == "metadata" {
				require.NoError(t, err)
				require.NotNil(t, got)
				require.Equal(t, map[string]any{"new": "preserved"}, r.a.Credentials["model_mapping"])
			} else {
				require.Error(t, err)
				require.Nil(t, got)
			}
		})
	}
}

func TestCredentialBrokerConcurrentNodesDoNotRotateTwice(t *testing.T) {
	s, r, c, e, _ := brokerFixture(t)
	first, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.NoError(t, err)
	other := *s
	other.refresh = NewOAuthRefreshAPI(r, c)
	started, release := make(chan struct{}), make(chan struct{})
	e.before = func() { close(started); <-release }
	done := make(chan error, 1)
	go func() { _, err := s.Issue(context.Background(), brokerFixtureKey, 4, first.Revision); done <- err }()
	<-started
	_, err = other.Issue(context.Background(), brokerFixtureKey, 4, first.Revision)
	brokerErrorCode(t, err, 503, "refresh_in_progress")
	close(release)
	require.NoError(t, <-done)
	got, err := other.Issue(context.Background(), brokerFixtureKey, 4, first.Revision)
	require.NoError(t, err)
	require.Equal(t, "rotated-access", got.AccessToken)
	require.Equal(t, 1, e.calls)
}

func TestCredentialBrokerPersistenceErrorsAreNotLogged(t *testing.T) {
	s, r, _, _, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	r.casErr = errors.New("NEVER-LOG-REFRESH-TOKEN")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.Error(t, err)
	require.NotContains(t, logs.String(), "NEVER-LOG")
}

type brokerNativeOAuthClient struct {
	OpenAIOAuthClient
	before func()
}

func (c *brokerNativeOAuthClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	if c.before != nil {
		c.before()
	}
	return &openai.TokenResponse{AccessToken: "native-new-at", RefreshToken: "native-new-rt", ExpiresIn: 3600}, nil
}

func TestCredentialBrokerNativeRefreshSkipsEnrichmentAndRawLogs(t *testing.T) {
	s, r, _, _, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	oauth := NewOpenAIOAuthService(nil, &brokerNativeOAuthClient{})
	defer oauth.Stop()
	oauth.accountRepo = r
	var calls int
	oauth.SetPrivacyClientFactory(func(string) (*req.Client, error) { calls++; return nil, errors.New("NEVER-LOG-UPSTREAM-CANARY") })
	s.executor = NewOpenAITokenRefresher(oauth, r)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	got, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.NoError(t, err)
	require.Equal(t, "native-new-at", got.AccessToken)
	require.Zero(t, calls, "optional network enrichment must not sit between rotation and persistence")
	require.NotContains(t, logs.String(), "NEVER-LOG")
}

func TestCredentialBrokerNativeRotationPersistsAfterDisconnect(t *testing.T) {
	for _, stage := range []string{"after-exchange", "during-CAS"} {
		t.Run(stage, func(t *testing.T) {
			s, r, c, _, _ := brokerFixture(t)
			r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &brokerNativeOAuthClient{}
			if stage == "after-exchange" {
				client.before = cancel
			} else {
				r.cas = cancel
			}
			oauth := NewOpenAIOAuthService(nil, client)
			defer oauth.Stop()
			oauth.accountRepo = r
			s.executor = NewOpenAITokenRefresher(oauth, r)
			got, err := s.Issue(ctx, brokerFixtureKey, 4, "")
			require.Error(t, err)
			require.Nil(t, got)
			require.Equal(t, "native-new-rt", r.a.GetCredential("refresh_token"), "a completed exchange must not lose the rotated RT on caller cancellation")
			require.Positive(t, c.deleted)
			require.False(t, c.deleteCanceled)
			require.True(t, c.deletedUnderLock)
		})
	}
}

func TestCredentialBrokerLostLeaseDoesNotPersist(t *testing.T) {
	s, r, c, e, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	e.before = func() { c.mu.Lock(); c.held = false; c.mu.Unlock() }
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.Error(t, err)
	require.Zero(t, r.writes)
}

func TestCredentialBrokerRetriesCacheInvalidationOnNextIssue(t *testing.T) {
	s, r, c, _, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	c.deleteErr = errors.New("redis temporarily down")
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.Error(t, err)
	require.Equal(t, "rotated-refresh", r.a.GetCredential("refresh_token"))
	before := c.deleted
	c.deleteErr = nil
	_, err = s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.NoError(t, err)
	require.Greater(t, c.deleted, before)
}

type brokerOwnedTestCache struct {
	brokerTestCache
	renewals int
}

func (c *brokerOwnedTestCache) AcquireOwnedRefreshLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	ok, err := c.AcquireRefreshLock(ctx, key, ttl)
	return "fixture-owner", ok, err
}
func (c *brokerOwnedTestCache) ReleaseOwnedRefreshLock(ctx context.Context, key, owner string) error {
	return c.ReleaseRefreshLock(ctx, key)
}
func (c *brokerOwnedTestCache) ExtendOwnedRefreshLock(context.Context, string, string, time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.renewals++
	return c.held, nil
}

func TestCredentialBrokerRefreshRenewsOwnedLease(t *testing.T) {
	s, r, _, e, _ := brokerFixture(t)
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	c := &brokerOwnedTestCache{}
	s.cache = c
	s.refresh = NewOAuthRefreshAPI(r, c, 90*time.Millisecond)
	e.before = func() { time.Sleep(130 * time.Millisecond) }
	_, err := s.Issue(context.Background(), brokerFixtureKey, 4, "")
	require.NoError(t, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.GreaterOrEqual(t, c.renewals, 2)
}

func TestCredentialBrokerShadowConversionDoesNotReadParent(t *testing.T) {
	_, r, _, _, _ := brokerFixture(t)
	a, err := r.GetByID(context.Background(), 4)
	require.NoError(t, err)
	ctx := withCredentialBrokerSnapshot(withCredentialBrokerRefresh(WithNativeCredentialMutation(context.Background())), a)
	parent := int64(14)
	r.a.ParentAccountID = &parent
	_, err = currentHomeCredentialOwner(ctx, r, a)
	require.ErrorIs(t, err, ErrCredentialBrokerStateChanged)
	require.Equal(t, []int64{4, 4}, r.queriedIDs)
}
