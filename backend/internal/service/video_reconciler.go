package service

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	videoErrorRouteAssignmentAbandoned = "ROUTE_ASSIGNMENT_ABANDONED"
	videoErrorStoredAccountMismatch    = "STORED_ACCOUNT_MISMATCH"
	videoErrorStoredProviderMissing    = "STORED_PROVIDER_UNAVAILABLE"
	videoErrorRecoveryNotFound         = "RECOVERY_NOT_FOUND"
	videoErrorPollAttemptsExhausted    = "POLL_ATTEMPTS_EXHAUSTED"
	videoErrorSettlementSnapshot       = "SETTLEMENT_SNAPSHOT_INVALID"
	videoErrorSettlementExceedsHold    = "SETTLEMENT_COST_EXCEEDS_HOLD"
)

type VideoReconcileRepository interface {
	ReleaseAndMarkSubmissionFailed(context.Context, ReleaseAndFailVideoSubmissionParams) (*VideoTask, error)
	RenewLease(context.Context, RenewVideoTaskLeaseParams) error
	ApplyPollResult(context.Context, ApplyVideoPollResultParams) (*VideoTask, error)
	ApplyRecoveredSubmission(context.Context, RecoverVideoSubmissionParams) (*VideoTask, error)
	ScheduleRetry(context.Context, ScheduleVideoTaskRetryParams) error
	MarkSettled(context.Context, MarkVideoSettledParams) error
}

type VideoAccountLookup interface {
	GetByID(context.Context, int64) (*Account, error)
}

type VideoReconcileBilling interface {
	Capture(context.Context, VideoTask, float64) error
	Release(context.Context, VideoTask) error
}

type VideoReconciler struct {
	repo          VideoReconcileRepository
	accounts      VideoAccountLookup
	billing       VideoReconcileBilling
	providers     *VideoProviderRegistry
	config        config.VideoConfig
	now           func() time.Time
	jitter        func(string, int, time.Duration) time.Duration
	leaseDuration time.Duration
	renewInterval time.Duration
}

func NewVideoReconciler(
	repo VideoReconcileRepository,
	accounts VideoAccountLookup,
	billing VideoReconcileBilling,
	providers *VideoProviderRegistry,
	cfg config.VideoConfig,
) *VideoReconciler {
	leaseDuration := time.Duration(cfg.LeaseSeconds) * time.Second
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	return &VideoReconciler{
		repo: repo, accounts: accounts, billing: billing, providers: providers, config: cfg,
		now: time.Now, jitter: deterministicVideoRetryJitter,
		leaseDuration: leaseDuration, renewInterval: leaseDuration / 3,
	}
}

func (r *VideoReconciler) Process(ctx context.Context, task VideoTask) error {
	if r == nil || r.repo == nil || r.accounts == nil || r.billing == nil || r.providers == nil {
		return ErrVideoServiceUnavailable
	}
	owner := strings.TrimSpace(valueOrEmptyVideoString(task.LeaseOwner))
	if owner == "" || task.LeaseExpiresAt == nil {
		return ErrVideoTaskLeaseConflict
	}

	switch task.Status {
	case VideoTaskCreated:
		return r.abandonRoutePending(ctx, task)
	case VideoTaskSubmitting:
		return r.recoverSubmission(ctx, task)
	case VideoTaskUnknown:
		if task.UpstreamTaskID == nil || strings.TrimSpace(*task.UpstreamTaskID) == "" {
			return r.recoverSubmission(ctx, task)
		}
		return r.poll(ctx, task)
	case VideoTaskSubmitted, VideoTaskQueued, VideoTaskRunning:
		return r.poll(ctx, task)
	case VideoTaskSucceeded, VideoTaskFailed, VideoTaskCancelled:
		if task.SettledAt != nil || task.SettledAmount != nil {
			return ErrVideoTaskInvalidTransition
		}
		return r.settle(ctx, task)
	default:
		return ErrVideoTaskInvalidTransition
	}
}

func (r *VideoReconciler) abandonRoutePending(ctx context.Context, task VideoTask) error {
	if task.AccountID != 0 || strings.TrimSpace(task.UpstreamModel) != "" || task.ProviderSubmissionToken != nil || task.UpstreamTaskID != nil {
		return ErrVideoTaskInvalidTransition
	}
	_, err := r.repo.ReleaseAndMarkSubmissionFailed(ctx, ReleaseAndFailVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, ExpectedStatus: VideoTaskCreated,
		Error: NewVideoTaskError(videoErrorRouteAssignmentAbandoned, "", false), FailedAt: r.currentTime(),
	})
	return err
}

func (r *VideoReconciler) recoverSubmission(ctx context.Context, task VideoTask) error {
	if task.Status != VideoTaskSubmitting && task.Status != VideoTaskUnknown {
		return ErrVideoTaskInvalidTransition
	}
	token := strings.TrimSpace(valueOrEmptyVideoString(task.ProviderSubmissionToken))
	if token == "" || task.UpstreamTaskID != nil || task.AccountID <= 0 || strings.TrimSpace(task.UpstreamModel) == "" {
		return r.scheduleRecoveryReview(ctx, task, NewVideoTaskError(videoErrorRecoveryNotFound, "", true))
	}
	account, provider, taskError := r.storedRoute(ctx, task)
	if taskError.Code() != "" {
		return r.scheduleRecoveryReview(ctx, task, taskError)
	}

	var result VideoSubmitResult
	var found bool
	err := r.withLeaseRenewal(ctx, task, func(callCtx context.Context) error {
		var callErr error
		result, found, callErr = provider.RecoverSubmission(callCtx, account, task, token)
		return callErr
	})
	if err != nil {
		return r.scheduleRecoveryReview(ctx, task, videoTaskErrorForReconciliation(err, "RECOVERY_FAILED"))
	}
	if !found || strings.TrimSpace(result.UpstreamTaskID) == "" || result.UpstreamTaskID == task.RequestID {
		return r.scheduleRecoveryReview(ctx, task, NewVideoTaskError(videoErrorRecoveryNotFound, "", true))
	}
	now := r.currentTime()
	nextPollAt := result.NextPollAt
	if nextPollAt == nil {
		next := now.Add(r.pollInterval())
		nextPollAt = &next
	}
	_, err = r.repo.ApplyRecoveredSubmission(ctx, RecoverVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		UpstreamTaskID: strings.TrimSpace(result.UpstreamTaskID), UpstreamStatus: strings.TrimSpace(result.UpstreamStatus),
		NextPollAt: nextPollAt, SubmittedAt: now, UpdatedAt: now,
	})
	return err
}

func (r *VideoReconciler) poll(ctx context.Context, task VideoTask) error {
	account, provider, taskError := r.storedRoute(ctx, task)
	if taskError.Code() != "" {
		return r.schedulePollReview(ctx, task, taskError, false)
	}
	if r.pollAttemptsExhausted(task.PollAttempts) {
		return r.schedulePollReview(ctx, task, NewVideoTaskError(videoErrorPollAttemptsExhausted, "", false), false)
	}
	var result VideoPollResult
	err := r.withLeaseRenewal(ctx, task, func(callCtx context.Context) error {
		var callErr error
		result, callErr = provider.Poll(callCtx, account, task)
		return callErr
	})
	if err != nil {
		return r.handlePollError(ctx, task, err)
	}
	if !validVideoPollTransitionStatus(result.Status) {
		return r.handlePollError(ctx, task, NewVideoProviderError(502, "provider_contract_error", false, false, nil))
	}
	if !IsTerminalVideoTaskStatus(result.Status) && r.pollAttemptsExhausted(task.PollAttempts+1) {
		return r.schedulePollReview(ctx, task, NewVideoTaskError(videoErrorPollAttemptsExhausted, "", false), true)
	}

	now := r.currentTime()
	nextPollAt := result.NextPollAt
	if nextPollAt == nil {
		next := now.Add(r.pollInterval())
		if IsTerminalVideoTaskStatus(result.Status) {
			next = now
		}
		nextPollAt = &next
	}
	params := ApplyVideoPollResultParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: result.Status, UpstreamStatus: strings.TrimSpace(result.UpstreamStatus), NextPollAt: nextPollAt,
		ResultURL: optionalVideoString(result.ResultURL), ResultURLExpiresAt: result.ResultURLExpiresAt,
		ResultContentType: optionalVideoString(result.ResultContentType), ResultWidth: optionalVideoInt(result.Width), ResultHeight: optionalVideoInt(result.Height),
		UpdatedAt: now,
	}
	if result.ActualDurationSeconds >= 0 && (result.ActualDurationSeconds > 0 || result.Status == VideoTaskSucceeded) {
		duration := float64(result.ActualDurationSeconds)
		params.ResultDurationSeconds = &duration
	}
	if result.Error.Code() != "" {
		taskError := result.Error
		params.Error = &taskError
	} else if result.Status == VideoTaskFailed || result.Status == VideoTaskCancelled {
		taskError := NewVideoTaskError("UPSTREAM_TASK_FAILED", "", false)
		params.Error = &taskError
	}
	updated, err := r.repo.ApplyPollResult(ctx, params)
	if err != nil {
		return err
	}
	if updated != nil && IsTerminalVideoTaskStatus(updated.Status) {
		return r.settle(ctx, *updated)
	}
	return nil
}

func (r *VideoReconciler) handlePollError(ctx context.Context, task VideoTask, err error) error {
	providerError, classified := classifyVideoProviderError(err)
	if classified && !providerError.Retryable {
		return r.terminalizePollFailure(ctx, task, NewVideoTaskError(providerError.Code, "", false))
	}
	return r.schedulePollRetryOrFail(ctx, task, videoTaskErrorForReconciliation(err, "POLL_FAILED"))
}

func (r *VideoReconciler) schedulePollRetryOrFail(ctx context.Context, task VideoTask, taskError VideoTaskError) error {
	if r.pollAttemptsExhausted(task.PollAttempts + 1) {
		return r.schedulePollReview(ctx, task, NewVideoTaskError(videoErrorPollAttemptsExhausted, "", false), true)
	}
	now := r.currentTime()
	return r.repo.ScheduleRetry(ctx, ScheduleVideoTaskRetryParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: task.Status, Error: taskError, NextPollAt: now.Add(r.retryDelay(task.RequestID, task.PollAttempts)),
		IncrementPollAttempts: true, UpdatedAt: now,
	})
}

func (r *VideoReconciler) pollAttemptsExhausted(attempts int) bool {
	return r.config.MaxPollAttempts <= 0 || attempts >= r.config.MaxPollAttempts
}

func (r *VideoReconciler) schedulePollReview(ctx context.Context, task VideoTask, taskError VideoTaskError, incrementPoll bool) error {
	now := r.currentTime()
	return r.repo.ScheduleRetry(ctx, ScheduleVideoTaskRetryParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: VideoTaskUnknown, Error: taskError, NextPollAt: now.Add(r.reviewDelay()),
		IncrementPollAttempts: incrementPoll, UpdatedAt: now,
	})
}

func (r *VideoReconciler) terminalizePollFailure(ctx context.Context, task VideoTask, taskError VideoTaskError) error {
	now := r.currentTime()
	terminal, err := r.repo.ApplyPollResult(ctx, ApplyVideoPollResultParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: VideoTaskFailed, UpstreamStatus: "failed", NextPollAt: &now, Error: &taskError,
		FinishedAt: &now, UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	if terminal == nil {
		return ErrVideoServiceUnavailable
	}
	return r.settle(ctx, *terminal)
}

func (r *VideoReconciler) settle(ctx context.Context, task VideoTask) error {
	now := r.currentTime()
	params := MarkVideoSettledParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner), SettledAt: now,
	}
	var billingErr error
	switch task.Status {
	case VideoTaskFailed, VideoTaskCancelled:
		billingErr = r.withLeaseRenewal(ctx, task, func(callCtx context.Context) error {
			return r.billing.Release(callCtx, task)
		})
		params.SettledAmount = 0
		params.BillingStatus = "released"
		params.BillingReference = videoStringPointer(VideoReleaseRequestID(task.RequestID))
	case VideoTaskSucceeded:
		amount, discrepancy, err := storedVideoSettlementAmount(task)
		if err != nil {
			return r.scheduleSettlementReview(ctx, task, NewVideoTaskError(videoErrorSettlementSnapshot, "", false))
		}
		billingErr = r.withLeaseRenewal(ctx, task, func(callCtx context.Context) error {
			return r.billing.Capture(callCtx, task, amount)
		})
		params.SettledAmount = amount
		params.BillingStatus = "captured"
		params.BillingReference = videoStringPointer(VideoCaptureRequestID(task.RequestID))
		if discrepancy {
			params.Error = NewVideoTaskError(videoErrorSettlementExceedsHold, "", false)
		}
	default:
		return ErrVideoTaskInvalidTransition
	}
	if billingErr != nil {
		return r.scheduleSettlementRetry(ctx, task, videoTaskErrorForReconciliation(billingErr, "SETTLEMENT_BILLING_FAILED"))
	}
	return r.repo.MarkSettled(ctx, params)
}

func (r *VideoReconciler) scheduleSettlementRetry(ctx context.Context, task VideoTask, taskError VideoTaskError) error {
	now := r.currentTime()
	return r.repo.ScheduleRetry(ctx, ScheduleVideoTaskRetryParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: task.Status, Error: taskError, NextPollAt: now.Add(r.retryDelay(task.RequestID, task.SettlementAttempts)),
		IncrementSettlementAttempts: true, UpdatedAt: now,
	})
}

func (r *VideoReconciler) scheduleSettlementReview(ctx context.Context, task VideoTask, taskError VideoTaskError) error {
	now := r.currentTime()
	return r.repo.ScheduleRetry(ctx, ScheduleVideoTaskRetryParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: task.Status, Error: taskError, NextPollAt: now.Add(r.reviewDelay()),
		IncrementSettlementAttempts: true, UpdatedAt: now,
	})
}

func (r *VideoReconciler) scheduleRecoveryReview(ctx context.Context, task VideoTask, taskError VideoTaskError) error {
	now := r.currentTime()
	return r.repo.ScheduleRetry(ctx, ScheduleVideoTaskRetryParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
		Status: VideoTaskUnknown, Error: taskError, NextPollAt: now.Add(r.reviewDelay()), UpdatedAt: now,
	})
}

func (r *VideoReconciler) reviewDelay() time.Duration {
	delay := time.Duration(r.config.UnknownReviewAfterHours) * time.Hour
	if delay <= 0 {
		return 24 * time.Hour
	}
	return delay
}

func (r *VideoReconciler) storedRoute(ctx context.Context, task VideoTask) (*Account, VideoProvider, VideoTaskError) {
	if task.AccountID <= 0 || strings.TrimSpace(task.Provider) == "" || strings.TrimSpace(task.UpstreamModel) == "" ||
		(task.Platform != PlatformVideo && task.Platform != PlatformGrok) ||
		(task.Platform == PlatformGrok && task.Provider != PlatformGrok) {
		return nil, nil, NewVideoTaskError(videoErrorStoredAccountMismatch, "", true)
	}
	account, err := r.accounts.GetByID(ctx, task.AccountID)
	if err != nil || account == nil || account.ID != task.AccountID || account.Platform != task.Platform || storedVideoAccountProvider(account) != task.Provider {
		return nil, nil, NewVideoTaskError(videoErrorStoredAccountMismatch, "", true)
	}
	if err := ValidateVideoAccountConfig(account.Platform, account.Type, account.Extra, account.Credentials); err != nil {
		return nil, nil, NewVideoTaskError(videoErrorStoredAccountMismatch, "", true)
	}
	provider, ok := r.providers.Get(task.Provider)
	if !ok || provider == nil || provider.Name() != task.Provider {
		return nil, nil, NewVideoTaskError(videoErrorStoredProviderMissing, "", true)
	}
	return account, provider, VideoTaskError{}
}

func storedVideoAccountProvider(account *Account) string {
	if account == nil {
		return ""
	}
	if account.Platform == PlatformGrok {
		return PlatformGrok
	}
	return account.VideoProvider()
}

func (r *VideoReconciler) withLeaseRenewal(ctx context.Context, task VideoTask, call func(context.Context) error) error {
	if call == nil {
		return ErrVideoTaskInvalidRequest
	}
	interval := r.renewInterval
	if interval <= 0 {
		interval = r.leaseDuration / 3
	}
	if interval <= 0 {
		interval = 20 * time.Second
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	renewErr := make(chan error, 1)
	ticker := time.NewTicker(interval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-callCtx.Done():
				return
			case <-ticker.C:
				err := r.repo.RenewLease(callCtx, RenewVideoTaskLeaseParams{
					RequestID: task.RequestID, ExpectedVersion: task.Version, LeaseOwner: valueOrEmptyVideoString(task.LeaseOwner),
					LeaseDuration: r.leaseDuration, UpdatedAt: r.currentTime(),
				})
				if err != nil {
					select {
					case renewErr <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	callErr := call(callCtx)
	cancel()
	<-done
	select {
	case err := <-renewErr:
		return err
	default:
		return callErr
	}
}

func (r *VideoReconciler) retryDelay(requestID string, attempt int) time.Duration {
	base := time.Duration(r.config.RetryBaseSeconds) * time.Second
	maximum := time.Duration(r.config.RetryMaxSeconds) * time.Second
	if base <= 0 {
		base = 5 * time.Second
	}
	if maximum < base {
		maximum = base
	}
	if attempt < 0 {
		attempt = 0
	}
	delay := base
	for i := 0; i < attempt && delay < maximum; i++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if r.jitter != nil {
		delay = r.jitter(requestID, attempt, delay)
	}
	if delay < base {
		return base
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func deterministicVideoRetryJitter(requestID string, attempt int, delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(requestID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.Itoa(attempt)))
	// Spread each task/attempt deterministically across 80%-120% of the
	// exponential delay, so process restarts do not synchronize retries.
	unit := float64(hash.Sum64()>>11) / float64(uint64(1)<<53)
	return time.Duration(float64(delay) * (0.8 + 0.4*unit))
}

func (r *VideoReconciler) pollInterval() time.Duration {
	interval := time.Duration(r.config.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		return 10 * time.Second
	}
	return interval
}

func (r *VideoReconciler) currentTime() time.Time {
	if r != nil && r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

func storedVideoSettlementAmount(task VideoTask) (float64, bool, error) {
	if task.UnitPrice < 0 || math.IsNaN(task.UnitPrice) || math.IsInf(task.UnitPrice, 0) ||
		task.FrozenAmount < 0 || math.IsNaN(task.FrozenAmount) || math.IsInf(task.FrozenAmount, 0) {
		return 0, false, ErrVideoTaskInvalidRequest
	}
	units := 1.0
	switch task.PricingUnit {
	case videoPricingPerRequest:
	case videoPricingPerSecond:
		if task.ResultDurationSeconds == nil || *task.ResultDurationSeconds <= 0 || math.IsNaN(*task.ResultDurationSeconds) || math.IsInf(*task.ResultDurationSeconds, 0) {
			return 0, false, ErrVideoTaskInvalidRequest
		}
		units = *task.ResultDurationSeconds
	default:
		return 0, false, ErrVideoTaskInvalidRequest
	}
	computed := canonicalVideoMoney(task.UnitPrice * units)
	frozen := canonicalVideoMoney(task.FrozenAmount)
	if math.IsNaN(computed) || math.IsInf(computed, 0) {
		return 0, false, ErrVideoTaskInvalidRequest
	}
	if computed > frozen {
		return frozen, true, nil
	}
	return computed, false, nil
}

func canonicalVideoMoney(value float64) float64 {
	return math.Round(value*1e8) / 1e8
}

func classifyVideoProviderError(err error) (VideoProviderError, bool) {
	var value VideoProviderError
	if errors.As(err, &value) {
		return value, true
	}
	var pointer *VideoProviderError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	return VideoProviderError{}, false
}

func videoTaskErrorForReconciliation(err error, fallback string) VideoTaskError {
	if providerError, ok := classifyVideoProviderError(err); ok {
		return NewVideoTaskError(providerError.Code, "", providerError.Retryable)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewVideoTaskError("UPSTREAM_TIMEOUT", "", true)
	}
	return NewVideoTaskError(fallback, "", true)
}

func validVideoPollTransitionStatus(status VideoTaskStatus) bool {
	switch status {
	case VideoTaskQueued, VideoTaskRunning, VideoTaskSucceeded, VideoTaskFailed, VideoTaskCancelled, VideoTaskUnknown:
		return true
	default:
		return false
	}
}

func optionalVideoString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func optionalVideoInt(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

func valueOrEmptyVideoString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
