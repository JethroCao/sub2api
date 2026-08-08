package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestVideoReconcilerSuccessSettlesOnce(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.provider.pollResult = VideoPollResult{Status: VideoTaskSucceeded, UpstreamStatus: "completed", ResultURL: "https://cdn.example.com/v.mp4", ActualDurationSeconds: 6}
	h.task.PricingUnit = videoPricingPerSecond
	h.task.UnitPrice = 0.1
	h.task.FrozenAmount = 1
	h.repo.task = h.task

	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.provider.pollCalls)
	require.Equal(t, 1, h.repo.applyCalls)
	require.Equal(t, 1, h.billing.captureCalls)
	require.Equal(t, 0, h.billing.releaseCalls)
	require.Equal(t, 1, h.repo.settleCalls)
	require.InDelta(t, 0.6, h.repo.settled.SettledAmount, 1e-9)
	require.Equal(t, "captured", h.repo.settled.BillingStatus)
	require.Equal(t, VideoCaptureRequestID(h.task.RequestID), derefVideoString(h.repo.settled.BillingReference))
}

func TestVideoReconcilerFailureAndCancellationReleaseOnce(t *testing.T) {
	for _, status := range []VideoTaskStatus{VideoTaskFailed, VideoTaskCancelled} {
		t.Run(string(status), func(t *testing.T) {
			h := newVideoReconcilerHarness(t)
			h.provider.pollResult = VideoPollResult{Status: status, UpstreamStatus: string(status), Error: NewVideoTaskError("CONTENT_REJECTED", "", false)}
			require.NoError(t, h.reconciler.Process(context.Background(), h.task))
			require.Equal(t, 1, h.billing.releaseCalls)
			require.Equal(t, 0, h.billing.captureCalls)
			require.Equal(t, 1, h.repo.settleCalls)
			require.Zero(t, h.repo.settled.SettledAmount)
			require.Equal(t, "released", h.repo.settled.BillingStatus)
		})
	}
}

func TestVideoReconcilerCostAboveHoldCapturesOnlyHoldAndFlagsAdminReview(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.provider.pollResult = VideoPollResult{Status: VideoTaskSucceeded, ActualDurationSeconds: 20}
	h.task.PricingUnit = videoPricingPerSecond
	h.task.UnitPrice = 0.2
	h.task.FrozenAmount = 1.5
	h.repo.task = h.task
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, []float64{1.5}, h.billing.capturedAmounts)
	require.Equal(t, "SETTLEMENT_COST_EXCEEDS_HOLD", h.repo.settled.Error.Code())
	require.InDelta(t, 1.5, h.repo.settled.SettledAmount, 1e-9)
}

func TestVideoReconcilerRetryablePollUsesInjectedCappedBackoff(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.task.PollAttempts = 50
	h.provider.pollErr = NewVideoProviderError(http.StatusServiceUnavailable, "upstream_unavailable", true, false, nil)
	h.reconciler.jitter = func(time.Duration) time.Duration { return 37 * time.Second }
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.repo.retryCalls)
	require.WithinDuration(t, h.now.Add(37*time.Second), h.repo.retry.NextPollAt, time.Nanosecond)
	require.True(t, h.repo.retry.IncrementPollAttempts)
	require.Zero(t, h.repo.applyCalls)
}

func TestVideoReconcilerNonRetryablePollTerminalizesAndReleases(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.provider.pollErr = NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, nil)
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.repo.applyCalls)
	require.Equal(t, VideoTaskFailed, h.repo.applied.Status)
	require.Equal(t, 1, h.billing.releaseCalls)
	require.Equal(t, 1, h.repo.settleCalls)
}

func TestVideoReconcilerUnknownRecoversWithoutSubmit(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	token := "vsub_recover"
	h.task.Status = VideoTaskUnknown
	h.task.UpstreamTaskID = nil
	h.task.ProviderSubmissionToken = &token
	h.provider.recoverFound = true
	h.provider.recoverResult = VideoSubmitResult{UpstreamTaskID: "upstream-recovered", Status: VideoTaskSubmitted, UpstreamStatus: "queued"}

	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 0, h.provider.submitCalls)
	require.Equal(t, 1, h.provider.recoverCalls)
	require.Equal(t, 1, h.repo.recoverCalls)
	require.Equal(t, "upstream-recovered", h.repo.recovered.UpstreamTaskID)
	require.Zero(t, h.provider.pollCalls)
}

func TestVideoReconcilerUnknownNotFoundKeepsHoldAndSchedulesReview(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	token := "vsub_missing"
	h.task.Status = VideoTaskUnknown
	h.task.UpstreamTaskID = nil
	h.task.ProviderSubmissionToken = &token
	h.provider.recoverFound = false
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 0, h.provider.submitCalls)
	require.Equal(t, 1, h.provider.recoverCalls)
	require.Equal(t, 1, h.repo.retryCalls)
	require.WithinDuration(t, h.now.Add(24*time.Hour), h.repo.retry.NextPollAt, time.Nanosecond)
	require.Equal(t, VideoTaskUnknown, h.repo.retry.Status)
	require.Zero(t, h.billing.captureCalls)
	require.Zero(t, h.billing.releaseCalls)
}

func TestVideoReconcilerCreatedRoutePendingOnlyReleases(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.task.Status = VideoTaskCreated
	h.task.AccountID = 0
	h.task.UpstreamModel = ""
	h.task.UpstreamTaskID = nil
	h.task.ProviderSubmissionToken = nil
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.repo.releaseSubmissionCalls)
	require.Zero(t, h.provider.submitCalls)
	require.Zero(t, h.provider.recoverCalls)
	require.Zero(t, h.provider.pollCalls)
}

func TestVideoReconcilerSubmittingOnlyRecovers(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	token := "vsub_submitting"
	h.task.Status = VideoTaskSubmitting
	h.task.UpstreamTaskID = nil
	h.task.ProviderSubmissionToken = &token
	h.provider.recoverFound = false
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Zero(t, h.provider.submitCalls)
	require.Equal(t, 1, h.provider.recoverCalls)
	require.Equal(t, VideoTaskUnknown, h.repo.retry.Status)
}

func TestVideoReconcilerStoredAccountMismatchFailsClosed(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.account.ID = h.task.AccountID + 1
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Zero(t, h.provider.pollCalls)
	require.Zero(t, h.provider.recoverCalls)
	require.Zero(t, h.provider.submitCalls)
	require.Equal(t, 1, h.repo.retryCalls)
	require.Equal(t, "STORED_ACCOUNT_MISMATCH", h.repo.retry.Error.Code())
}

func TestVideoReconcilerUsesStoredGrokAccountWithoutSchedulerFailover(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.task.Platform = PlatformGrok
	h.task.Provider = PlatformGrok
	h.repo.task = h.task
	h.account.Platform = PlatformGrok
	h.account.Extra = nil
	h.account.Credentials = map[string]any{"api_key": "grok-key"}
	h.provider.name = PlatformGrok
	registry, err := NewVideoProviderRegistry(h.provider)
	require.NoError(t, err)
	h.reconciler.providers = registry

	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.provider.pollCalls)
	require.Zero(t, h.repo.retryCalls)
}

func TestVideoReconcilerTerminalUnsettledReplaysStableCapture(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.task.Status = VideoTaskSucceeded
	duration := 5.0
	h.task.ResultDurationSeconds = &duration
	h.task.PricingUnit = videoPricingPerSecond
	h.task.UnitPrice = 0.1
	h.task.FrozenAmount = 1
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.billing.captureCalls)
	require.Equal(t, 1, h.repo.settleCalls)
	require.Zero(t, h.provider.pollCalls)
}

func TestVideoReconcilerRenewsLeaseWhileProviderCallIsInFlight(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	started := make(chan struct{})
	release := make(chan struct{})
	h.provider.pollFn = func(ctx context.Context, _ *Account, _ VideoTask) (VideoPollResult, error) {
		close(started)
		select {
		case <-release:
			return VideoPollResult{Status: VideoTaskRunning}, nil
		case <-ctx.Done():
			return VideoPollResult{}, ctx.Err()
		}
	}
	h.reconciler.leaseDuration = 30 * time.Millisecond
	h.reconciler.renewInterval = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- h.reconciler.Process(context.Background(), h.task) }()
	<-started
	require.Eventually(t, func() bool { return h.repo.renewCount() > 0 }, time.Second, time.Millisecond)
	close(release)
	require.NoError(t, <-done)
}

func TestVideoReconcilerRenewsLeaseWhileBillingIsInFlight(t *testing.T) {
	h := newVideoReconcilerHarness(t)
	h.task.Status = VideoTaskSucceeded
	duration := 5.0
	h.task.ResultDurationSeconds = &duration
	started := make(chan struct{})
	release := make(chan struct{})
	h.billing.captureFn = func(ctx context.Context, _ VideoTask, _ float64) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.reconciler.leaseDuration = 30 * time.Millisecond
	h.reconciler.renewInterval = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- h.reconciler.Process(context.Background(), h.task) }()
	<-started
	require.Eventually(t, func() bool { return h.repo.renewCount() > 0 }, time.Second, time.Millisecond)
	close(release)
	require.NoError(t, <-done)
}

type videoReconcilerHarness struct {
	now        time.Time
	task       VideoTask
	account    *Account
	repo       *fakeVideoReconcileRepository
	provider   *fakeVideoReconcileProvider
	billing    *fakeVideoReconcileBilling
	reconciler *VideoReconciler
}

func newVideoReconcilerHarness(t *testing.T) *videoReconcilerHarness {
	t.Helper()
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	owner := "worker-test"
	upstreamID := "upstream-task"
	task := VideoTask{
		RequestID: "vid_0123456789abcdef0123456789abcdef", UserID: 1, APIKeyID: 2, GroupID: 3,
		AccountID: 4, Platform: PlatformVideo, Provider: VideoProviderSeedance,
		Operation: "generation", ExternalModel: "seedance", UpstreamModel: "seedance-upstream",
		Status: VideoTaskSubmitted, UpstreamTaskID: &upstreamID, PricingUnit: videoPricingPerRequest,
		UnitPrice: 0.5, FrozenAmount: 1, BillingMode: "balance", BillingStatus: "held",
		LeaseOwner: &owner, LeaseExpiresAt: videoTimePointer(now.Add(time.Hour)), Version: 7,
	}
	repo := &fakeVideoReconcileRepository{task: task}
	provider := &fakeVideoReconcileProvider{name: VideoProviderSeedance, pollResult: VideoPollResult{Status: VideoTaskRunning}}
	registry, err := NewVideoProviderRegistry(provider)
	require.NoError(t, err)
	account := &Account{
		ID: task.AccountID, Platform: PlatformVideo, Type: AccountTypeAPIKey,
		Extra: map[string]any{"video_provider": VideoProviderSeedance}, Credentials: map[string]any{"api_key": "test-key"},
	}
	accounts := &fakeVideoAccountLookup{account: account}
	billing := &fakeVideoReconcileBilling{}
	reconciler := NewVideoReconciler(repo, accounts, billing, registry, config.VideoConfig{
		LeaseSeconds: 60, PollIntervalSeconds: 10, RetryBaseSeconds: 5, RetryMaxSeconds: 300,
		MaxPollAttempts: 720, UnknownReviewAfterHours: 24,
	})
	reconciler.now = func() time.Time { return now }
	reconciler.jitter = func(d time.Duration) time.Duration { return d }
	reconciler.renewInterval = time.Hour
	return &videoReconcilerHarness{now: now, task: task, account: account, repo: repo, provider: provider, billing: billing, reconciler: reconciler}
}

type fakeVideoReconcileRepository struct {
	mu                     sync.Mutex
	task                   VideoTask
	applyCalls             int
	applied                ApplyVideoPollResultParams
	recoverCalls           int
	recovered              RecoverVideoSubmissionParams
	retryCalls             int
	retry                  ScheduleVideoTaskRetryParams
	settleCalls            int
	settled                MarkVideoSettledParams
	releaseSubmissionCalls int
	renewCalls             int
}

func (r *fakeVideoReconcileRepository) ApplyPollResult(_ context.Context, p ApplyVideoPollResultParams) (*VideoTask, error) {
	r.applyCalls++
	r.applied = p
	r.task.Status = p.Status
	r.task.Version++
	r.task.ResultDurationSeconds = p.ResultDurationSeconds
	return &r.task, nil
}
func (r *fakeVideoReconcileRepository) ApplyRecoveredSubmission(_ context.Context, p RecoverVideoSubmissionParams) (*VideoTask, error) {
	r.recoverCalls++
	r.recovered = p
	r.task.Status = VideoTaskSubmitted
	r.task.UpstreamTaskID = &p.UpstreamTaskID
	r.task.Version++
	return &r.task, nil
}
func (r *fakeVideoReconcileRepository) ScheduleRetry(_ context.Context, p ScheduleVideoTaskRetryParams) error {
	r.retryCalls++
	r.retry = p
	return nil
}
func (r *fakeVideoReconcileRepository) MarkSettled(_ context.Context, p MarkVideoSettledParams) error {
	r.settleCalls++
	r.settled = p
	return nil
}
func (r *fakeVideoReconcileRepository) ReleaseAndMarkSubmissionFailed(_ context.Context, p ReleaseAndFailVideoSubmissionParams) (*VideoTask, error) {
	r.releaseSubmissionCalls++
	r.task.Status = VideoTaskFailed
	return &r.task, nil
}
func (r *fakeVideoReconcileRepository) RenewLease(context.Context, RenewVideoTaskLeaseParams) error {
	r.mu.Lock()
	r.renewCalls++
	r.mu.Unlock()
	return nil
}
func (r *fakeVideoReconcileRepository) renewCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renewCalls
}

type fakeVideoAccountLookup struct {
	account *Account
	err     error
}

func (r *fakeVideoAccountLookup) GetByID(context.Context, int64) (*Account, error) {
	return r.account, r.err
}

type fakeVideoReconcileBilling struct {
	captureCalls    int
	releaseCalls    int
	capturedAmounts []float64
	captureErr      error
	releaseErr      error
	captureFn       func(context.Context, VideoTask, float64) error
}

func (b *fakeVideoReconcileBilling) Capture(ctx context.Context, task VideoTask, amount float64) error {
	b.captureCalls++
	b.capturedAmounts = append(b.capturedAmounts, amount)
	if b.captureFn != nil {
		return b.captureFn(ctx, task, amount)
	}
	return b.captureErr
}
func (b *fakeVideoReconcileBilling) Release(context.Context, VideoTask) error {
	b.releaseCalls++
	return b.releaseErr
}

type fakeVideoReconcileProvider struct {
	name          string
	submitCalls   int
	recoverCalls  int
	pollCalls     int
	recoverFound  bool
	recoverResult VideoSubmitResult
	recoverErr    error
	pollResult    VideoPollResult
	pollErr       error
	pollFn        func(context.Context, *Account, VideoTask) (VideoPollResult, error)
}

func (p *fakeVideoReconcileProvider) Name() string { return p.name }
func (p *fakeVideoReconcileProvider) Capabilities() VideoProviderCapabilities {
	return VideoProviderCapabilities{}
}
func (p *fakeVideoReconcileProvider) Submit(context.Context, *Account, CanonicalVideoRequest, string) (VideoSubmitResult, error) {
	p.submitCalls++
	return VideoSubmitResult{}, errors.New("Submit must never be called by reconciliation")
}
func (p *fakeVideoReconcileProvider) RecoverSubmission(context.Context, *Account, VideoTask, string) (VideoSubmitResult, bool, error) {
	p.recoverCalls++
	return p.recoverResult, p.recoverFound, p.recoverErr
}
func (p *fakeVideoReconcileProvider) Poll(ctx context.Context, account *Account, task VideoTask) (VideoPollResult, error) {
	p.pollCalls++
	if p.pollFn != nil {
		return p.pollFn(ctx, account, task)
	}
	return p.pollResult, p.pollErr
}
func (p *fakeVideoReconcileProvider) OpenContent(context.Context, *Account, VideoTask) (io.ReadCloser, http.Header, int64, error) {
	return nil, nil, 0, errors.New("unused")
}

func videoTimePointer(value time.Time) *time.Time { return &value }
func derefVideoString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
