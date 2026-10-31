package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/stretchr/testify/require"
)

type homeCredentialRaceRepo struct {
	AccountRepository
	current  *Account
	disabled []int64
}

func (r *homeCredentialRaceRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.current, nil
}
func (r *homeCredentialRaceRepo) SetError(_ context.Context, id int64, _ string) error {
	r.disabled = append(r.disabled, id)
	return nil
}

func TestHomeRegression401Rebind(t *testing.T) {
	first, second := homeTestAccount(), homeTestAccount()
	first.Credentials = map[string]any{"access_token": "rejected-first"}
	second.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
	repo := &homeCredentialRaceRepo{current: second}
	client := &homeAuxiliaryClient{}
	home := NewHomeCredentialsService(repo, client)
	limiter := NewRateLimitService(repo, nil, nil, nil, nil)
	limiter.homeCredentials = home
	require.True(t, limiter.handleHome401(context.Background(), first, http.StatusUnauthorized))
	require.Empty(t, client.observations(), "old binding 401 must not refresh the replacement")
}
func TestHomeRegression401Unbind(t *testing.T) {
	first := homeTestAccount()
	first.Credentials = map[string]any{"access_token": "rejected-first"}
	native := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "new-native", "refresh_token": "new-native-refresh"}}
	repo := &homeCredentialRaceRepo{current: native}
	limiter := NewRateLimitService(repo, nil, nil, nil, nil)
	limiter.homeCredentials = NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
	limiter.HandleUpstreamError(context.Background(), first, http.StatusUnauthorized, nil, []byte(`{"error":{"code":"token_revoked"}}`))
	require.Empty(t, repo.disabled, "old Home request must not disable replacement native credentials")
}

type homeCredentialRaceClient struct {
	mu            sync.Mutex
	observations  []string
	started       chan struct{}
	release       chan struct{}
	old, new      homeclient.Credential
	oldErr        error
	forcedStarted chan struct{}
	forcedRelease chan struct{}
	forcedOnce    sync.Once
}

func (*homeCredentialRaceClient) Ready() bool { return true }
func (c *homeCredentialRaceClient) Refresh(_ context.Context, _ string, observed string) (homeclient.Credential, error) {
	c.mu.Lock()
	c.observations = append(c.observations, observed)
	c.mu.Unlock()
	if observed != "" {
		if c.forcedStarted != nil {
			c.forcedOnce.Do(func() { close(c.forcedStarted) })
			<-c.forcedRelease
		}
		return c.new, nil
	}
	close(c.started)
	<-c.release
	return c.old, c.oldErr
}
func (c *homeCredentialRaceClient) Acquire(context.Context, homeclient.Request) (*homeclient.Lease, error) {
	return &homeclient.Lease{Credential: c.new}, nil
}
func TestHomeLateAuxiliaryRefreshCannotReplaceAdmissionCredential(t *testing.T) {
	for _, oldErr := range []error{nil, errors.New("late Home outage")} {
		name := "success"
		if oldErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			c := &homeCredentialRaceClient{started: make(chan struct{}), release: make(chan struct{}), oldErr: oldErr, old: homeclient.Credential{ID: testHomeID, AccessToken: "old-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, new: homeclient.Credential{ID: testHomeID, AccessToken: "new-at", AccountID: "workspace", ExpiresAt: time.Now().Add(2 * time.Hour)}}
			home := NewHomeCredentialsService(nil, c)
			done := make(chan error, 1)
			go func() { _, err := home.Resolve(context.Background(), homeTestAccount()); done <- err }()
			<-c.started
			_, err := home.Acquire(context.Background(), homeTestAccount(), homeclient.Request{Model: "gpt-test"})
			require.NoError(t, err)
			close(c.release)
			require.NoError(t, <-done)
			token, err := home.Resolve(context.Background(), homeTestAccount())
			require.NoError(t, err)
			require.Equal(t, "new-at", token.AccessToken, "earlier refresh success/error must not replace a later admitted token")
		})
	}
}
func TestHomeRegressionRecoveryJoinsNonForcedRefresh(t *testing.T) {
	c := &homeCredentialRaceClient{started: make(chan struct{}), release: make(chan struct{}), old: homeclient.Credential{ID: testHomeID, AccessToken: "rejected-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, new: homeclient.Credential{ID: testHomeID, AccessToken: "recovered-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, c)
	done := make(chan error, 1)
	go func() { _, err := home.Resolve(context.Background(), homeTestAccount()); done <- err }()
	<-c.started
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, home.Recover(canceled, homeTestAccount(), "rejected-at"), context.Canceled)
	close(c.release)
	<-done
	cached, err := home.Resolve(context.Background(), homeTestAccount())
	require.NoError(t, err)
	require.NotEqual(t, "rejected-at", cached.AccessToken, "401 invalidation must survive an older in-flight cache fill")
}
func TestHomePrivacy401RecoversWithoutReplaying(t *testing.T) {
	for _, operation := range []string{"ensure", "force"} {
		t.Run(operation, func(t *testing.T) {
			a := homeTestAccount()
			repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{7: a}}
			client := &homeAuxiliaryClient{refresh: func(id, observed string) (homeclient.Credential, error) {
				token := "rejected-at"
				if observed != "" {
					token = "recovered-at"
				}
				return homeclient.Credential{ID: id, AccessToken: token, AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}}
			home := NewHomeCredentialsService(repo, client)
			var sent []string
			svc := &adminServiceImpl{accountRepo: repo, homeCredentials: home, privacyClientFactory: homeAuxiliaryHTTPFactory(func(r *http.Request) (int, string) {
				sent = append(sent, r.Header.Get("Authorization"))
				if len(sent) == 1 {
					return http.StatusUnauthorized, `{}`
				}
				return http.StatusOK, `{}`
			})}
			call := svc.ForceOpenAIPrivacy
			if operation == "ensure" {
				call = svc.EnsureOpenAIPrivacy
			}
			require.Equal(t, PrivacyModeFailed, call(context.Background(), a))
			require.Equal(t, []string{"Bearer rejected-at"}, sent, "recovery must not replay the privacy operation")
			require.Equal(t, []string{"", "rejected-at"}, client.observations())
			require.Equal(t, PrivacyModeTrainingOff, call(context.Background(), a))
			require.Equal(t, []string{"Bearer rejected-at", "Bearer recovered-at"}, sent)
			require.Len(t, client.observations(), 2)
		})
	}
}

func TestHomeManagedShadow401ContextSurvivesUnbind(t *testing.T) {
	owner := homeTestAccount()
	shadow := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID}
	repo := &homeMetadataRepo{accounts: map[int64]*Account{7: owner, 8: shadow}}
	home := NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
	gateway := &OpenAIGatewayService{accountRepo: repo, homeCredentials: home}
	ctx, _, _, err := gateway.getOpenAIRequestAccessToken(context.Background(), nil, shadow)
	require.NoError(t, err)
	repo.replace(&Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "replacement-native-at", "refresh_token": "replacement-native-rt"}})
	limiter := NewRateLimitService(repo, nil, nil, nil, nil)
	limiter.homeCredentials = home
	require.False(t, limiter.HandleUpstreamError(ctx, shadow, http.StatusUnauthorized, nil, []byte(`{"error":{"code":"token_revoked"}}`)))
	require.Zero(t, repo.errors.Load(), "Home shadow rejection must not disable its replacement native owner")
}

func TestHomeCredentialGenerationSurvivesEvictionAndReentry(t *testing.T) {
	client := &homeCredentialRaceClient{started: make(chan struct{}), release: make(chan struct{}), old: homeclient.Credential{ID: testHomeID, AccessToken: "old-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, client)
	done := make(chan error, 1)
	go func() { _, err := home.Resolve(context.Background(), homeTestAccount()); done <- err }()
	<-client.started
	for i := range 2050 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		a := homeTestAccount()
		a.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: id}
		client.new = homeclient.Credential{ID: id, AccessToken: "other-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}
		_, err := home.Acquire(context.Background(), a, homeclient.Request{Model: "gpt-test"})
		require.NoError(t, err)
	}
	client.new = homeclient.Credential{ID: testHomeID, AccessToken: "replacement-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}
	_, err := home.Acquire(context.Background(), homeTestAccount(), homeclient.Request{Model: "gpt-test"})
	require.NoError(t, err)
	close(client.release)
	require.NoError(t, <-done)
	token, err := home.Resolve(context.Background(), homeTestAccount())
	require.NoError(t, err)
	require.Equal(t, "replacement-at", token.AccessToken, "an old in-flight generation must not match an evicted/recreated credential state")
}

func TestHomeCanceledRecoveryRetainsWorkerOwnershipPastCooldown(t *testing.T) {
	client := &homeCredentialRaceClient{forcedStarted: make(chan struct{}), forcedRelease: make(chan struct{}), new: homeclient.Credential{ID: testHomeID, AccessToken: "recovered-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, client)
	require.NoError(t, home.cacheCredential(testHomeID, homeclient.Credential{ID: testHomeID, AccessToken: "rejected-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, home.Recover(canceled, homeTestAccount(), "rejected-at"), context.Canceled)
	<-client.forcedStarted
	home.mu.Lock()
	attempt := home.recoveries[testHomeID]
	attempt.until = time.Now().Add(-time.Second)
	home.recoveries[testHomeID] = attempt
	home.mu.Unlock()
	// A second canceled observer should be absorbed by the running worker, not
	// become a new recovery waiter when the five-second backoff has elapsed.
	err := home.Recover(canceled, homeTestAccount(), "rejected-at")
	close(client.forcedRelease)
	require.NoError(t, err)
	token, err := home.Resolve(context.Background(), homeTestAccount())
	require.NoError(t, err)
	require.Equal(t, "recovered-at", token.AccessToken)
}

type homeResponseMutationRepo struct {
	*homeMetadataRepo
	mutations int
}

func (r *homeResponseMutationRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.mutations++
	return nil
}
func (r *homeResponseMutationRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.mutations++
	return nil
}

func TestHomeOld403And429CannotMutateReplacementOwner(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		for _, transition := range []string{"rebind", "unbind", "same"} {
			for _, selection := range []string{"owner", "shadow_parent_callback"} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, transition, selection), func(t *testing.T) {
					owner := homeTestAccount()
					repo := &homeResponseMutationRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}}
					selected := owner
					if selection == "shadow_parent_callback" {
						selected = &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID}
						repo.replace(selected)
					}
					home := NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
					limiter := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
					limiter.homeCredentials = home
					gateway := &OpenAIGatewayService{accountRepo: repo, homeCredentials: home, rateLimitService: limiter}
					limiter.SetAccountRuntimeBlocker(gateway)
					ctx, _, _, err := gateway.getOpenAIRequestAccessToken(context.Background(), nil, selected)
					require.NoError(t, err)
					current := homeTestAccount()
					if transition == "rebind" {
						current.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
					}
					if transition == "unbind" {
						current.Extra = nil
						current.Credentials = map[string]any{"access_token": "replacement-at", "refresh_token": "replacement-rt"}
					}
					repo.replace(current)
					headers := http.Header{}
					var body []byte
					if status == http.StatusForbidden {
						body = []byte(`{"error":{"code":"account_deactivated","message":"Account deactivated"}}`)
					} else {
						headers.Set("x-codex-primary-used-percent", "100")
						headers.Set("x-codex-primary-reset-after-seconds", "600")
						headers.Set("x-codex-primary-window-minutes", "300")
					}
					gateway.handleOpenAIAccountUpstreamError(ctx, owner, status, headers, body)
					limiter.HandleUpstreamError(ctx, owner, status, headers, body)
					if transition == "same" {
						require.Positive(t, int(repo.errors.Load())+repo.mutations, "same-binding account errors must still apply normal policy")
						require.True(t, gateway.isOpenAIAccountRuntimeBlocked(owner))
					} else {
						require.Zero(t, repo.errors.Load())
						require.Zero(t, repo.mutations, "old response must not persist quota/error state to replacement")
						require.False(t, gateway.isOpenAIAccountRuntimeBlocked(owner))
					}
				})
			}
		}
	}
}

type homeRejectedWriteRepo struct {
	*homeResponseMutationRepo
	replacement *Account
}

func (r *homeRejectedWriteRepo) SetError(context.Context, int64, string) error {
	r.replace(r.replacement)
	return ErrHomeOwnershipChanged
}
func (r *homeRejectedWriteRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.replace(r.replacement)
	return ErrHomeOwnershipChanged
}
func (r *homeRejectedWriteRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.replace(r.replacement)
	return ErrHomeOwnershipChanged
}

func TestHomeRejectedStateWriteDoesNotPublishRuntimeBlock(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			owner := homeTestAccount()
			native := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "replacement-at", "refresh_token": "replacement-rt"}}
			repo := &homeRejectedWriteRepo{homeResponseMutationRepo: &homeResponseMutationRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}}, replacement: native}
			home := NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
			limiter := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			limiter.homeCredentials = home
			gateway := &OpenAIGatewayService{accountRepo: repo, homeCredentials: home, rateLimitService: limiter}
			limiter.SetAccountRuntimeBlocker(gateway)
			ctx, _, _, err := gateway.getOpenAIRequestAccessToken(context.Background(), nil, owner)
			require.NoError(t, err)
			headers := http.Header{}
			headers.Set("x-codex-primary-used-percent", "100")
			headers.Set("x-codex-primary-reset-after-seconds", "600")
			headers.Set("x-codex-primary-window-minutes", "300")
			body := []byte(`{"error":{"code":"account_deactivated"}}`)
			if status == 429 {
				body = nil
			}
			gateway.handleOpenAIAccountUpstreamError(ctx, owner, status, headers, body)
			_, published := gateway.openaiAccountRuntimeBlockUntil.Load(owner.ID)
			require.False(t, published, "a rejected conditional DB write must not publish any runtime block")
		})
	}
}

func TestHomeRuntimeBlockKeepsCredentialIdentity(t *testing.T) {
	for _, shadowed := range []bool{false, true} {
		t.Run(fmt.Sprint(shadowed), func(t *testing.T) {
			owner := homeTestAccount()
			repo := &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}
			selected := owner
			if shadowed {
				selected = &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &owner.ID}
				repo.replace(selected)
			}
			snapshot := ApplyHomeCredential(selected, owner, homeclient.Credential{ID: testHomeID, AccessToken: "old-at", AccountID: "workspace"})
			gateway := &OpenAIGatewayService{accountRepo: repo}
			gateway.BlockAccountScheduling(snapshot, time.Now().Add(time.Hour), "old-result")
			replacement := homeTestAccount()
			replacement.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
			repo.replace(replacement)
			require.False(t, gateway.isOpenAIAccountRuntimeBlocked(selected), "old UUID block must not apply after binding changes")
			newSnapshot := ApplyHomeCredential(selected, replacement, homeclient.Credential{ID: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e", AccessToken: "new-at", AccountID: "new-workspace"})
			gateway.BlockAccountScheduling(newSnapshot, time.Now().Add(time.Minute), "current-result")
			gateway.BlockAccountScheduling(snapshot, time.Now().Add(time.Hour), "late-old-result")
			require.True(t, gateway.isOpenAIAccountRuntimeBlocked(selected), "late old results must not erase a newer valid block")
		})
	}
}

type home403CounterTrap struct {
	OpenAI403CounterCache
	calls int
}

func (c *home403CounterTrap) IncrementOpenAI403Count(context.Context, int64, int) (int64, error) {
	c.calls++
	return 1, nil
}

type homeAsyncSnapshotRepo struct {
	AccountRepository
	received chan string
}

func (r *homeAsyncSnapshotRepo) UpdateExtra(ctx context.Context, _ int64, _ map[string]any) error {
	id, _ := HomeResultCredentialID(ctx)
	r.received <- id
	return ErrHomeOwnershipChanged
}

func TestHome403DoesNotPublishUnversionedRedisStrikes(t *testing.T) {
	owner := homeTestAccount()
	repo := &homeResponseMutationRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}}
	limiter := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	trap := &home403CounterTrap{}
	limiter.SetOpenAI403CounterCache(trap)
	// Actual Home execution is reported to Home for cooldown. Its ordinary403
	// cannot add a local strike that could later disable another bound UUID.
	limiter.HandleUpstreamError(WithHomeResultCredentialID(context.Background(), testHomeID), owner, http.StatusForbidden, nil, []byte(`{"error":{"message":"temporary rejection"}}`))
	require.Zero(t, trap.calls)
}
func TestHomeAsynchronousQuotaSnapshotRetainsBindingFence(t *testing.T) {
	repo := &homeAsyncSnapshotRepo{received: make(chan string, 1)}
	svc := &OpenAIGatewayService{accountRepo: repo}
	ctx := context.WithValue(context.Background(), homeRequestCredentialKey{}, homeRequestCredential{accountID: 7, credential: homeclient.Credential{ID: testHomeID}})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "50")
	headers.Set("x-codex-primary-reset-after-seconds", "600")
	headers.Set("x-codex-primary-window-minutes", "300")
	svc.UpdateCodexUsageSnapshotFromHeaders(canceled, 7, headers)
	select {
	case id := <-repo.received:
		require.Equal(t, testHomeID, id)
	case <-time.After(time.Second):
		t.Fatal("async snapshot did not reach local repository")
	}
}

func (r *homeResponseMutationRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.mutations++
	return nil
}

func TestHomeBackgroundOwnershipChangeIsSkippedWithoutPenalty(t *testing.T) {
	for _, unified := range []bool{false, true} {
		t.Run(fmt.Sprint(unified), func(t *testing.T) {
			current := homeTestAccount()
			repo := &homeResponseMutationRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{7: current}}}
			stale := *current
			stale.Extra = nil
			svc := &TokenRefreshService{accountRepo: repo, cfg: &config.TokenRefreshConfig{MaxRetries: 1}}
			client := &openaiOAuthClientRefreshStub{}
			oauth := &OpenAIOAuthService{accountRepo: repo, oauthClient: client}
			refresher := NewOpenAITokenRefresher(oauth, repo)
			var executor OAuthRefreshExecutor
			if unified {
				svc.refreshAPI = NewOAuthRefreshAPI(repo, nil)
				executor = refresher
			}
			err := svc.refreshWithRetry(context.Background(), &stale, refresher, executor, time.Minute)
			require.ErrorIs(t, err, errRefreshSkipped)
			require.Zero(t, client.refreshCalls)
			require.Zero(t, repo.mutations, "skipped native work must not penalize the new Home owner")
		})
	}
}

func TestHomeRuntimePublicationWaitsForOuterCommit(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprint(commit), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectBegin()
			tx, err := client.Tx(context.Background())
			require.NoError(t, err)
			owner := homeTestAccount()
			repo := &homeMetadataRepo{accounts: map[int64]*Account{7: owner}}
			limiter := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			gateway := &OpenAIGatewayService{accountRepo: repo, rateLimitService: limiter}
			limiter.SetAccountRuntimeBlocker(gateway)
			ctx := WithHomeResultCredentialID(dbent.NewTxContext(context.Background(), tx), testHomeID)
			gateway.handleOpenAIAccountUpstreamError(ctx, owner, http.StatusForbidden, nil, []byte(`{"error":{"code":"account_deactivated"}}`))
			_, before := gateway.openaiAccountRuntimeBlockUntil.Load(owner.ID)
			if commit {
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
			} else {
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
			}
			_, after := gateway.openaiAccountRuntimeBlockUntil.Load(owner.ID)
			require.False(t, before, "caller-owned transactions must not publish runtime state before commit")
			require.Equal(t, commit, after, "rollback must not publish Home runtime state")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

type homeDelayedAdmissionClient struct {
	homeCredentialClient
	started, release chan struct{}
}

func (c *homeDelayedAdmissionClient) Acquire(ctx context.Context, r homeclient.Request) (*homeclient.Lease, error) {
	lease, err := c.homeCredentialClient.Acquire(ctx, r)
	if err != nil {
		return nil, err
	}
	close(c.started)
	<-c.release
	return lease, nil
}
func (c *homeDelayedAdmissionClient) Refresh(_ context.Context, id, observed string) (homeclient.Credential, error) {
	return homeclient.Credential{ID: id, AccessToken: "recovered-at", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func TestHomeDelayedAdmissionCannotResurrectRejectedToken(t *testing.T) {
	_, gateway := homeTransportSetup(t)
	delayed := &homeDelayedAdmissionClient{homeCredentialClient: gateway.homeCredentials.client, started: make(chan struct{}), release: make(chan struct{})}
	home := NewHomeCredentialsService(nil, delayed)
	type result struct {
		lease *homeclient.Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, err := home.Acquire(context.Background(), homeTransportAccount(), homeclient.Request{Model: transportHomeTestModel})
		done <- result{lease, err}
	}()
	<-delayed.started
	require.NoError(t, home.Recover(context.Background(), homeTransportAccount(), "access-token"))
	close(delayed.release)
	got := <-done
	if got.lease != nil {
		got.lease.Finish(homeclient.Result{Failed: true})
	}
	require.Error(t, got.err, "an already-issued admission with a known rejected token must not be sent upstream")
	cached, err := home.Resolve(context.Background(), homeTransportAccount())
	require.NoError(t, err)
	require.Equal(t, "recovered-at", cached.AccessToken)
}

type homePrivacyOwnershipRepo struct {
	*homeMetadataRepo
	writes int
}

func (r *homePrivacyOwnershipRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.accounts[id]
	if expected, ok := HomeResultCredentialID(ctx); ok {
		actual, _ := current.HomeCredentialID()
		if expected != actual {
			return ErrHomeOwnershipChanged
		}
	}
	r.writes++
	if current.Extra == nil {
		current.Extra = map[string]any{}
	}
	for k, v := range updates {
		current.Extra[k] = v
	}
	return nil
}
func TestHomePrivacyResultDoesNotMarkReplacementIdentity(t *testing.T) {
	for _, op := range []string{"ensure", "force"} {
		for _, transition := range []string{"same", "rebind", "unbind", "native"} {
			t.Run(op+"/"+transition, func(t *testing.T) {
				account := homeTestAccount()
				account.Credentials = map[string]any{"access_token": "native-at"}
				if transition == "native" {
					account.Extra = nil
				}
				repo := &homePrivacyOwnershipRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{7: account}}}
				client := &homeAuxiliaryClient{}
				svc := &adminServiceImpl{accountRepo: repo, homeCredentials: NewHomeCredentialsService(repo, client), privacyClientFactory: homeAuxiliaryHTTPFactory(func(*http.Request) (int, string) {
					if transition == "rebind" || transition == "unbind" {
						replacement := homeTestAccount()
						replacement.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
						if transition == "unbind" {
							replacement.Extra = nil
						}
						repo.replace(replacement)
					}
					return http.StatusOK, `{}`
				})}
				call := svc.EnsureOpenAIPrivacy
				if op == "force" {
					call = svc.ForceOpenAIPrivacy
				}
				require.Equal(t, PrivacyModeTrainingOff, call(context.Background(), account))
				if transition == "same" || transition == "native" {
					require.Equal(t, 1, repo.writes)
				} else {
					require.Zero(t, repo.writes, "privacy result from old token must not suppress Ensure on its replacement")
				}
			})
		}
	}
}

type homeTimeoutOwnershipRepo struct {
	*homeMetadataRepo
	writes      int
	unguarded   int
	beforeWrite func()
}

func (r *homeTimeoutOwnershipRepo) write(ctx context.Context, id int64) error {
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	expected, guarded := HomeResultCredentialID(ctx)
	if guarded {
		current, _ := r.GetByID(ctx, id)
		actual, _ := current.HomeCredentialID()
		if actual != expected {
			return ErrHomeOwnershipChanged
		}
	} else {
		r.unguarded++
	}
	r.writes++
	return nil
}
func (r *homeTimeoutOwnershipRepo) SetError(ctx context.Context, id int64, _ string) error {
	return r.write(ctx, id)
}
func (r *homeTimeoutOwnershipRepo) SetTempUnschedulable(ctx context.Context, id int64, _ time.Time, _ string) error {
	return r.write(ctx, id)
}

func TestHomeTimeoutCannotMutateReplacement(t *testing.T) {
	for _, action := range []string{"temp_unsched", "error"} {
		for _, transition := range []string{"rebind", "unbind", "same", "write_rebind"} {
			t.Run(action+"/"+transition, func(t *testing.T) {
				original := homeTestAccount()
				repo := &homeTimeoutOwnershipRepo{homeMetadataRepo: &homeMetadataRepo{accounts: map[int64]*Account{7: original}}}
				home := NewHomeCredentialsService(repo, &homeAuxiliaryClient{})
				settings := NewSettingService(&homeTimeoutSettingsRepo{values: map[string]string{
					SettingKeyStreamTimeoutSettings: fmt.Sprintf(`{"enabled":true,"action":%q,"temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":1}`, action),
				}}, nil)
				counter := &homeTimeoutCounterTrap{}
				limiter := &RateLimitService{accountRepo: repo, homeCredentials: home, settingService: settings, timeoutCounterCache: counter}
				gateway := &OpenAIGatewayService{accountRepo: repo, homeCredentials: home, rateLimitService: limiter}
				limiter.SetAccountRuntimeBlocker(gateway)
				ctx, _, _, err := gateway.getOpenAIRequestAccessToken(context.Background(), nil, original)
				require.NoError(t, err)
				require.True(t, homeRequestWasManaged(ctx, original.ID))
				_, guarded := HomeResultCredentialID(ctx)
				require.False(t, guarded, "normal inference token ctx contains request marker only")
				replacement := homeTestAccount()
				replacement.Extra = map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: "0b1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}
				if transition == "unbind" {
					replacement.Extra = nil
				}
				if transition == "write_rebind" {
					repo.beforeWrite = func() { repo.replace(replacement) }
				} else if transition != "same" {
					repo.replace(replacement)
				}
				limiter.HandleStreamTimeout(ctx, original, "gpt-test")
				if transition == "same" {
					require.Equal(t, 1, repo.writes)
					require.True(t, gateway.isOpenAIAccountRuntimeBlocked(original))
				} else {
					require.Zero(t, repo.writes, "old Home timeout changed replacement; unguarded writes=%d", repo.unguarded)
					_, published := gateway.openaiAccountRuntimeBlockUntil.Load(original.ID)
					require.False(t, published)
				}
				require.Zero(t, repo.unguarded)
				require.Zero(t, counter.calls)
			})
		}
	}
}

type homeExpiredRecoveryClient struct {
	current      homeclient.Credential
	observations []string
}

func (*homeExpiredRecoveryClient) Ready() bool { return true }
func (*homeExpiredRecoveryClient) Acquire(context.Context, homeclient.Request) (*homeclient.Lease, error) {
	return nil, ErrHomeUnavailable
}
func (c *homeExpiredRecoveryClient) Refresh(_ context.Context, _ string, observed string) (homeclient.Credential, error) {
	c.observations = append(c.observations, observed)
	// Actual Home v1.1.7 AuthIsNewerThanObserved short-circuits before
	// refresh when the observed hash differs, regardless of current expiry.
	if observed != "" && observed != c.current.AccessToken {
		return c.current, nil
	}
	c.current.AccessToken = "rotated-after-expiry"
	c.current.ExpiresAt = time.Now().Add(time.Hour)
	return c.current, nil
}
func TestHomeRecoveredTokenExpiryCanRefresh(t *testing.T) {
	client := &homeExpiredRecoveryClient{current: homeclient.Credential{ID: testHomeID, AccessToken: "recovered-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, client)
	require.NoError(t, home.Recover(context.Background(), homeTestAccount(), "rejected-at"))
	client.current.ExpiresAt = time.Now().Add(-time.Second)
	home.mu.Lock()
	entry := home.cache[testHomeID]
	entry.until = time.Now().Add(-time.Second)
	home.cache[testHomeID] = entry
	home.mu.Unlock()
	credential, err := home.Resolve(context.Background(), homeTestAccount())
	require.NoError(t, err, "expired recovered token must request refresh instead of historical hash; observations=%v", client.observations)
	require.Equal(t, "rotated-after-expiry", credential.AccessToken)
}

type homeTimeoutSettingsRepo struct {
	SettingRepository
	values map[string]string
}

func (r *homeTimeoutSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	return r.values[key], nil
}

type homeCanceledReadRepo struct {
	AccountRepository
	current *Account
}

func (r *homeCanceledReadRepo) GetByID(ctx context.Context, _ int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.current, nil
}
func TestHomeCanceledRecoveryWithRepositoryInvalidatesRejection(t *testing.T) {
	original := homeTestAccount()
	client := &homeExpiredRecoveryClient{current: homeclient.Credential{ID: testHomeID, AccessToken: "recovered-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(&homeCanceledReadRepo{current: original}, client)
	require.NoError(t, home.cacheCredential(testHomeID, homeclient.Credential{ID: testHomeID, AccessToken: "rejected-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, home.Recover(ctx, original, "rejected-at"), context.Canceled)
	credential, err := home.Resolve(context.Background(), original)
	require.NoError(t, err)
	require.NotEqual(t, "rejected-at", credential.AccessToken, "real repository honors canceled ctx before Recover invalidates cache")
}

type homeTimeoutCounterTrap struct {
	TimeoutCounterCache
	calls int
}

func (c *homeTimeoutCounterTrap) IncrementTimeoutCount(context.Context, int64, int) (int64, error) {
	c.calls++
	return 1, nil
}
func (c *homeTimeoutCounterTrap) ResetTimeoutCount(context.Context, int64) error {
	c.calls++
	return nil
}

func TestHomeLateOldRejectionAfterNewTokenExpires(t *testing.T) {
	client := &homeExpiredRecoveryClient{current: homeclient.Credential{ID: testHomeID, AccessToken: "recovered-at", AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}}
	home := NewHomeCredentialsService(nil, client)
	require.NoError(t, home.Recover(context.Background(), homeTestAccount(), "rejected-at"))
	client.current.ExpiresAt = time.Now().Add(-time.Second)
	home.mu.Lock()
	entry := home.cache[testHomeID]
	entry.until = time.Now().Add(-time.Second)
	home.cache[testHomeID] = entry
	attempt := home.recoveries[testHomeID]
	attempt.until = time.Now().Add(-time.Second)
	home.recoveries[testHomeID] = attempt
	home.mu.Unlock()
	// An old in-flight request finally returns 401 after the newer AT expires.
	require.Error(t, home.Recover(context.Background(), homeTestAccount(), "rejected-at"))
	home.mu.Lock()
	failure := home.failures[testHomeID]
	failure.until = time.Now().Add(-time.Second)
	home.failures[testHomeID] = failure
	home.mu.Unlock()
	credential, err := home.Resolve(context.Background(), homeTestAccount())
	require.NoError(t, err, "failed old-token comparison must not pin future expiry refreshes; observations=%v", client.observations)
	require.Equal(t, "rotated-after-expiry", credential.AccessToken)
}
