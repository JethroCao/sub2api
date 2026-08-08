//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var videoRequestIDPattern = regexp.MustCompile(`^vid_[0-9a-f]{32}$`)

func TestVideoTaskRepositoryRequestIDFormat(t *testing.T) {
	repo := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "request-id", "")

	task, created, err := repo.CreateOrGet(context.Background(), params)
	require.NoError(t, err)
	require.True(t, created)
	require.Regexp(t, videoRequestIDPattern, task.RequestID)
	require.Nil(t, task.UpstreamTaskID)
	cleanupVideoTask(t, task.RequestID)
}

func TestVideoTaskRepositoryAssignsRouteAndPersistsSubmissionRecoveryState(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "submission-route", "")
	params.AccountID = 0
	params.UpstreamModel = ""
	routeRecoveryAt := time.Now().UTC().Add(time.Minute)
	params.NextPollAt = &routeRecoveryAt
	task, created, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, task.RequestID)

	err = repo.AssignAndMarkSubmitting(ctx, service.AssignVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version,
		AccountID: 93, Platform: service.PlatformVideo, Provider: service.VideoProviderSeedance,
		UpstreamModel: "seedance-upstream-v2", ProviderSubmissionToken: "safe-submit-token",
		NextPollAt: routeRecoveryAt.Add(time.Minute),
	})
	require.NoError(t, err)

	nextPollAt := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	err = repo.MarkSubmissionUnknownAt(ctx, service.MarkVideoSubmissionUnknownParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version + 1,
		Error: service.NewVideoTaskError("UPSTREAM_TIMEOUT", "", true), NextPollAt: nextPollAt,
		UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, int64(93), stored.AccountID)
	require.Equal(t, "seedance-upstream-v2", stored.UpstreamModel)
	require.Equal(t, service.VideoTaskUnknown, stored.Status)
	require.Equal(t, task.Version+2, stored.Version)
	require.Equal(t, 1, stored.SubmissionAttempts)
	require.Equal(t, "safe-submit-token", videoTaskStringValue(stored.ProviderSubmissionToken))
	require.WithinDuration(t, nextPollAt, *stored.NextPollAt, time.Microsecond)
	require.NotEmpty(t, stored.RequestPayload)

	leased, err := repo.LeaseDue(ctx, "worker-submission-recovery", 1, time.Minute, nextPollAt.Add(time.Second))
	require.NoError(t, err)
	require.Len(t, leased, 1, "unknown submissions without an upstream ID must remain recoverable")
	require.Equal(t, task.RequestID, leased[0].RequestID)
	require.Nil(t, leased[0].UpstreamTaskID)
}

func TestVideoTaskRepositoryAssignRouteRequiresPendingMatchingRoute(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)

	t.Run("already assigned", func(t *testing.T) {
		task, created, err := repo.CreateOrGet(ctx, videoTaskCreateParams(t, "route-already-assigned", ""))
		require.NoError(t, err)
		require.True(t, created)
		cleanupVideoTask(t, task.RequestID)

		err = repo.AssignAndMarkSubmitting(ctx, service.AssignVideoSubmissionParams{
			RequestID: task.RequestID, ExpectedVersion: task.Version,
			AccountID: 94, Platform: service.PlatformVideo, Provider: service.VideoProviderSeedance,
			UpstreamModel: "replacement", ProviderSubmissionToken: "safe-submit-token",
			NextPollAt: time.Now().UTC().Add(time.Minute),
		})
		require.ErrorIs(t, err, service.ErrVideoTaskInvalidTransition)
	})

	t.Run("provider mismatch", func(t *testing.T) {
		params := videoTaskCreateParams(t, "route-provider-mismatch", "")
		params.AccountID = 0
		params.UpstreamModel = ""
		nextPollAt := time.Now().UTC().Add(time.Minute)
		params.NextPollAt = &nextPollAt
		task, created, err := repo.CreateOrGet(ctx, params)
		require.NoError(t, err)
		require.True(t, created)
		cleanupVideoTask(t, task.RequestID)

		err = repo.AssignAndMarkSubmitting(ctx, service.AssignVideoSubmissionParams{
			RequestID: task.RequestID, ExpectedVersion: task.Version,
			AccountID: 95, Platform: service.PlatformVideo, Provider: service.VideoProviderKling,
			UpstreamModel: "wrong-provider", ProviderSubmissionToken: "safe-submit-token",
			NextPollAt: nextPollAt.Add(time.Minute),
		})
		require.ErrorIs(t, err, service.ErrVideoTaskInvalidTransition)
	})
}

func TestVideoTaskRepositoryLeasesCrashRecoveryStatesWithoutResubmissionIdentity(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	dueAt := time.Now().UTC().Add(-time.Minute)

	pendingParams := videoTaskCreateParams(t, "crash-route-pending", "")
	pendingParams.AccountID = 0
	pendingParams.UpstreamModel = ""
	pendingParams.NextPollAt = &dueAt
	pending, created, err := repo.CreateOrGet(ctx, pendingParams)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, pending.RequestID)

	assignedParams := videoTaskCreateParams(t, "crash-route-assigned", "")
	assignedParams.AccountID = 0
	assignedParams.UpstreamModel = ""
	assignedParams.NextPollAt = &dueAt
	assigned, created, err := repo.CreateOrGet(ctx, assignedParams)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, assigned.RequestID)
	require.NoError(t, repo.AssignAndMarkSubmitting(ctx, service.AssignVideoSubmissionParams{
		RequestID: assigned.RequestID, ExpectedVersion: assigned.Version,
		AccountID: 96, Platform: service.PlatformVideo, Provider: service.VideoProviderSeedance,
		UpstreamModel: "seedance-recovery", ProviderSubmissionToken: "recover-only-token",
		NextPollAt: dueAt,
	}))

	leased, err := repo.LeaseDue(ctx, "worker-crash-recovery", 50, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	leasedPending := findLeasedVideoTask(t, leased, pending.RequestID)
	require.Equal(t, service.VideoTaskCreated, leasedPending.Status)
	require.Zero(t, leasedPending.AccountID)
	require.Nil(t, leasedPending.ProviderSubmissionToken)
	require.Nil(t, leasedPending.UpstreamTaskID)
	leasedAssigned := findLeasedVideoTask(t, leased, assigned.RequestID)
	require.Equal(t, service.VideoTaskSubmitting, leasedAssigned.Status)
	require.Equal(t, "recover-only-token", videoTaskStringValue(leasedAssigned.ProviderSubmissionToken))
	require.Nil(t, leasedAssigned.UpstreamTaskID)
}

func TestVideoTaskRepositoryLeasedRoutePendingRecoveryOnlyReleasesAndTerminalizes(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	user := mustCreateUser(t, client, &service.User{
		Email: fmt.Sprintf("video-route-recovery-%d@example.com", time.Now().UnixNano()), PasswordHash: "hash", Balance: 10,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name: "video-route-recovery-" + videoTaskHash(t.Name())[:12], Platform: service.PlatformVideo,
		SubscriptionType: service.SubscriptionTypeStandard, AllowVideoGeneration: true,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID, GroupID: &group.ID, Key: "sk-video-route-recovery-" + videoTaskHash(t.Name())[:20], Name: "video-route-recovery",
	})
	dueAt := time.Now().UTC().Add(-time.Minute)
	params := videoTaskCreateParams(t, "leased-route-pending-release", "")
	params.UserID, params.APIKeyID, params.GroupID = user.ID, apiKey.ID, group.ID
	params.AccountID, params.UpstreamModel = 0, ""
	params.NextPollAt = &dueAt
	params.FrozenAmount, params.EstimatedAmount = 2, 2
	params.BillingStatus = "held"
	task, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, task.RequestID)
	assertVideoBalanceExact(t, user.ID, "8", "2")

	leased, err := repo.LeaseDue(ctx, "worker-route-recovery-release", 50, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	leasedTask := findLeasedVideoTask(t, leased, task.RequestID)
	require.Equal(t, service.VideoTaskCreated, leasedTask.Status)
	require.Nil(t, leasedTask.ProviderSubmissionToken)

	failed, err := repo.ReleaseAndMarkSubmissionFailed(ctx, service.ReleaseAndFailVideoSubmissionParams{
		RequestID: leasedTask.RequestID, ExpectedVersion: leasedTask.Version, ExpectedStatus: service.VideoTaskCreated,
		Error: service.NewVideoTaskError("ROUTE_ASSIGNMENT_ABANDONED", "", false), FailedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskFailed, failed.Status)
	require.Nil(t, failed.LeaseOwner)
	require.Nil(t, failed.NextPollAt)
	require.Nil(t, failed.ProviderSubmissionToken)
	assertVideoBalanceExact(t, user.ID, "10", "0")
}

func TestVideoTaskRepositorySubmissionFailureAtomicallyReleasesHoldAndClearsRecoveryData(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	billing := NewUsageBillingRepository(client, integrationDB)
	task, _, user := createVideoBillingTask(t, client, repo, "submission-failed", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))
	require.NoError(t, repo.MarkSubmitting(ctx, task.RequestID, task.Version, "safe-submit-token"))

	failed, err := repo.ReleaseAndMarkSubmissionFailed(ctx, service.ReleaseAndFailVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version + 1, ExpectedStatus: service.VideoTaskSubmitting,
		ProviderSubmissionToken: "safe-submit-token",
		Error:                   service.NewVideoTaskError("INVALID_REQUEST", "", false), FailedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskFailed, failed.Status)

	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskFailed, stored.Status)
	require.Equal(t, "released", stored.BillingStatus)
	require.Nil(t, stored.ProviderSubmissionToken)
	require.Empty(t, stored.RequestPayload)
	assertVideoBalanceExact(t, user.ID, "10", "0")

	replayed, err := repo.ReleaseAndMarkSubmissionFailed(ctx, service.ReleaseAndFailVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version + 1, ExpectedStatus: service.VideoTaskSubmitting,
		ProviderSubmissionToken: "safe-submit-token",
		Error:                   service.NewVideoTaskError("INVALID_REQUEST", "", false), FailedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Equal(t, stored.Version, replayed.Version)
	assertVideoBalanceExact(t, user.ID, "10", "0")
}

func TestVideoTaskRepositorySubmissionFailureRollsBackReleaseWhenTerminalMutationFails(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	billing := NewUsageBillingRepository(client, integrationDB)
	task, apiKey, user := createVideoBillingTask(t, client, repo, "submission-failed-rollback", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))
	require.NoError(t, repo.MarkSubmitting(ctx, task.RequestID, task.Version, "safe-submit-token"))

	_, err := integrationDB.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION video_task_test_reject_terminalization()
		RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.request_id = '`+task.RequestID+`' AND NEW.status = 'failed' THEN
				RAISE EXCEPTION 'injected terminal mutation failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_video_task_test_reject_terminalization
		BEFORE UPDATE ON video_tasks
		FOR EACH ROW EXECUTE FUNCTION video_task_test_reject_terminalization();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_video_task_test_reject_terminalization ON video_tasks`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS video_task_test_reject_terminalization()`)
	})

	_, err = repo.ReleaseAndMarkSubmissionFailed(ctx, service.ReleaseAndFailVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version + 1, ExpectedStatus: service.VideoTaskSubmitting,
		ProviderSubmissionToken: "safe-submit-token",
		Error:                   service.NewVideoTaskError("INVALID_REQUEST", "", false), FailedAt: time.Now().UTC(),
	})
	require.ErrorContains(t, err, "injected terminal mutation failure")
	assertVideoBalanceExact(t, user.ID, "8", "2")
	var releaseClaims int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2`, service.VideoReleaseRequestID(task.RequestID), apiKey.ID).Scan(&releaseClaims))
	require.Zero(t, releaseClaims)
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskSubmitting, stored.Status)
}

func TestCreateVideoTaskAndReserveIsAtomic(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)

	hold := 2.0
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("video-atomic-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      1,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name:                 "video-atomic-group-" + videoTaskHash(t.Name())[:12],
		Platform:             service.PlatformVideo,
		SubscriptionType:     service.SubscriptionTypeStandard,
		AllowVideoGeneration: true,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID:  user.ID,
		GroupID: &group.ID,
		Key:     "sk-video-atomic-" + videoTaskHash(t.Name())[:20],
		Name:    "video-atomic",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name:     "video-atomic-account-" + videoTaskHash(t.Name())[:12],
		Platform: service.PlatformVideo,
	})
	params := videoTaskCreateParams(t, "atomic-reserve", videoTaskHash("atomic-"+t.Name()))
	params.UserID = user.ID
	params.APIKeyID = apiKey.ID
	params.GroupID = group.ID
	params.AccountID = account.ID
	params.FrozenAmount = hold
	params.EstimatedAmount = hold
	params.Currency = ""

	_, _, err := repo.CreateTaskAndReserve(ctx, params)
	require.ErrorIs(t, err, service.ErrVideoInsufficientBalance)

	var taskCount, holdCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM video_tasks WHERE idempotency_key_hash = $1`, params.IdempotencyKeyHash).Scan(&taskCount))
	require.Zero(t, taskCount)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id = $1 AND request_id LIKE 'video_hold:%'`, apiKey.ID).Scan(&holdCount))
	require.Zero(t, holdCount)

	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET balance = 10 WHERE id = $1`, user.ID)
	require.NoError(t, err)
	task, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "USD", task.Currency)
	cleanupVideoTask(t, task.RequestID)

	replayed, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, task.RequestID, replayed.RequestID)

	var balance, frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen_balance FROM users WHERE id = $1`, user.ID).Scan(&balance, &frozen))
	require.InDelta(t, 8, balance, 0.000000001)
	require.InDelta(t, 2, frozen, 0.000000001)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id = $1 AND request_id = $2`, apiKey.ID, service.VideoHoldRequestID(task.RequestID)).Scan(&holdCount))
	require.Equal(t, 1, holdCount)
}

func TestCreateVideoTaskAndReserveCrossOperationKeyCreatesOneHold(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	user := mustCreateUser(t, client, &service.User{
		Email: fmt.Sprintf("video-cross-operation-hold-%d@example.com", time.Now().UnixNano()), PasswordHash: "hash", Balance: 10,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name: "video-cross-operation-hold-" + videoTaskHash(t.Name())[:12], Platform: service.PlatformVideo,
		SubscriptionType: service.SubscriptionTypeStandard, AllowVideoGeneration: true,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID, GroupID: &group.ID, Key: "sk-video-cross-operation-hold-" + videoTaskHash(t.Name())[:20], Name: "video-cross-operation-hold",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name: "video-cross-operation-hold-" + videoTaskHash("account-" + t.Name())[:12], Platform: service.PlatformVideo,
	})
	params := videoTaskCreateParams(t, "cross-operation-hold", videoTaskHash("cross-operation-hold-"+t.Name()))
	params.UserID, params.APIKeyID, params.GroupID, params.AccountID = user.ID, apiKey.ID, group.ID, account.ID
	params.FrozenAmount, params.EstimatedAmount = 2, 2
	params.BillingStatus = "held"

	task, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, task.RequestID)

	crossOperation := params
	crossOperation.Operation = "extension"
	crossOperation.RequestHash = videoTaskHash("cross-operation-extension-" + t.Name())
	_, _, err = repo.CreateTaskAndReserve(ctx, crossOperation)
	require.ErrorIs(t, err, service.ErrVideoIdempotencyConflict)

	replayed, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, task.RequestID, replayed.RequestID)
	assertVideoBalanceExact(t, user.ID, "8", "2")
	var tasks, holds int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tasks WHERE user_id = $1 AND api_key_id = $2 AND idempotency_key_hash = $3`, user.ID, apiKey.ID, params.IdempotencyKeyHash).Scan(&tasks))
	require.Equal(t, 1, tasks)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id = $1 AND request_id = $2`, apiKey.ID, service.VideoHoldRequestID(task.RequestID)).Scan(&holds))
	require.Equal(t, 1, holds)
}

func TestCreateVideoTaskAndReserveSubscriptionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	dailyLimit := 3.0
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("video-atomic-sub-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name:                 "video-atomic-sub-group-" + videoTaskHash(t.Name())[:12],
		Platform:             service.PlatformVideo,
		SubscriptionType:     service.SubscriptionTypeSubscription,
		DailyLimitUSD:        &dailyLimit,
		AllowVideoGeneration: true,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID:  user.ID,
		GroupID: &group.ID,
		Key:     "sk-video-atomic-sub-" + videoTaskHash(t.Name())[:20],
		Name:    "video-atomic-sub",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name:     "video-atomic-sub-account-" + videoTaskHash(t.Name())[:12],
		Platform: service.PlatformVideo,
	})
	subscription := mustCreateSubscription(t, client, &service.UserSubscription{
		UserID:        user.ID,
		GroupID:       group.ID,
		DailyUsageUSD: 0.5,
	})
	params := videoTaskCreateParams(t, "atomic-subscription", videoTaskHash("atomic-sub-"+t.Name()))
	params.UserID = user.ID
	params.APIKeyID = apiKey.ID
	params.SubscriptionID = &subscription.ID
	params.GroupID = group.ID
	params.AccountID = account.ID
	params.FrozenAmount = 2
	params.EstimatedAmount = 2
	params.BillingMode = "subscription"

	task, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, task.RequestID)
	replayed, created, err := repo.CreateTaskAndReserve(ctx, params)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, task.RequestID, replayed.RequestID)

	var balance, frozen, daily float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id = $1`, user.ID).Scan(&balance))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT frozen_quota, daily_usage_usd FROM user_subscriptions WHERE id = $1`, subscription.ID).Scan(&frozen, &daily))
	require.InDelta(t, 100, balance, 0.000000001)
	require.InDelta(t, 2, frozen, 0.000000001)
	require.InDelta(t, 0.5, daily, 0.000000001)
}

func TestVideoTaskRepositoryDefaultsEmptyCurrencyBeforeCreate(t *testing.T) {
	repo := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "empty-currency", "")
	params.Currency = ""

	task, created, err := repo.CreateOrGet(context.Background(), params)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "USD", task.Currency)
	cleanupVideoTask(t, task.RequestID)
}

func TestVideoTaskRepositoryCreateIdempotencyConflict(t *testing.T) {
	repoA := NewVideoTaskRepository(integrationDB)
	repoB := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "idempotency", videoTaskHash("same-key"+t.Name()))
	params.ExternalModel = "idempotency-overlap-test"
	installVideoTaskCreateOverlapTrigger(t)
	type result struct {
		task    *service.VideoTask
		created bool
		err     error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, repo := range []service.VideoTaskRepository{repoA, repoB} {
		wg.Add(1)
		go func(i int, repo service.VideoTaskRepository) {
			defer wg.Done()
			<-start
			results[i].task, results[i].created, results[i].err = repo.CreateOrGet(context.Background(), params)
		}(i, repo)
	}
	close(start)
	requireVideoTaskConcurrentInserts(t, 2)
	wg.Wait()
	for _, result := range results {
		require.NoError(t, result.err)
		require.NotNil(t, result.task)
	}
	require.NotEqual(t, results[0].created, results[1].created)
	require.Equal(t, results[0].task.RequestID, results[1].task.RequestID)
	cleanupVideoTask(t, results[0].task.RequestID)

	params.RequestHash = videoTaskHash("different-request")
	_, _, err := repoB.CreateOrGet(context.Background(), params)
	require.ErrorIs(t, err, service.ErrVideoIdempotencyConflict)
}

func TestVideoTaskRepositoryIdempotencyKeyIsScopedAcrossOperations(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "cross-operation-idempotency", videoTaskHash("cross-operation-"+t.Name()))
	original, created, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, original.RequestID)

	conflict := params
	conflict.Operation = "edit"
	conflict.RequestHash = videoTaskHash("edit-request-" + t.Name())
	_, _, err = repo.CreateOrGet(ctx, conflict)
	require.ErrorIs(t, err, service.ErrVideoIdempotencyConflict)

	replayed, created, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, original.RequestID, replayed.RequestID)

	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM video_tasks
		WHERE user_id = $1 AND api_key_id = $2 AND idempotency_key_hash = $3
	`, params.UserID, params.APIKeyID, params.IdempotencyKeyHash).Scan(&count))
	require.Equal(t, 1, count)
}

func TestVideoTaskRepositoryConcurrentCrossOperationIdempotencyCreatesOneTask(t *testing.T) {
	ctx := context.Background()
	repos := []service.VideoTaskRepository{NewVideoTaskRepository(integrationDB), NewVideoTaskRepository(integrationDB)}
	params := videoTaskCreateParams(t, "concurrent-cross-operation", videoTaskHash("concurrent-cross-operation-"+t.Name()))
	params.ExternalModel = "idempotency-overlap-test"
	requests := []service.CreateVideoTaskParams{params, params}
	requests[1].Operation = "extension"
	requests[1].RequestHash = videoTaskHash("concurrent-extension-" + t.Name())
	installVideoTaskCreateOverlapTrigger(t)

	type result struct {
		task    *service.VideoTask
		created bool
		err     error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].task, results[i].created, results[i].err = repos[i].CreateOrGet(ctx, requests[i])
		}(i)
	}
	close(start)
	requireVideoTaskConcurrentInserts(t, 2)
	wg.Wait()

	createdCount, conflictCount := 0, 0
	for _, result := range results {
		if result.created {
			createdCount++
			cleanupVideoTask(t, result.task.RequestID)
		}
		if errors.Is(result.err, service.ErrVideoIdempotencyConflict) {
			conflictCount++
		}
	}
	require.Equal(t, 1, createdCount)
	require.Equal(t, 1, conflictCount)
}

func installVideoTaskCreateOverlapTrigger(t *testing.T) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `
		CREATE OR REPLACE FUNCTION video_task_test_hold_create()
		RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.external_model = 'idempotency-overlap-test' THEN
				PERFORM pg_sleep(1);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_video_task_test_hold_create
		BEFORE INSERT ON video_tasks
		FOR EACH ROW EXECUTE FUNCTION video_task_test_hold_create();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_video_task_test_hold_create ON video_tasks`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS video_task_test_hold_create()`)
	})
}

func requireVideoTaskConcurrentInserts(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		var active int
		err := integrationDB.QueryRowContext(context.Background(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND position('INSERT INTO video_tasks (' in query) > 0
		`).Scan(&active)
		require.NoError(t, err)
		if active >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d concurrent video task inserts", want)
}

func TestVideoTaskRepositoryMinimizedPayloadRejectsCredentialsAndOversizeValues(t *testing.T) {
	_, err := service.NewMinimizedVideoPayload(map[string]any{
		"prompt":        "safe prompt",
		"authorization": "Bearer secret",
	})
	require.ErrorIs(t, err, service.ErrVideoTaskUnsafePayload)

	_, err = service.NewMinimizedVideoPayload(map[string]any{
		"prompt": string(make([]byte, service.MaxVideoTaskPayloadBytes+1)),
	})
	require.ErrorIs(t, err, service.ErrVideoTaskPayloadTooLarge)
}

func TestVideoTaskRepositoryMarkSubmittedClearsPayloadAndRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "submit", "")
	task, _, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)

	err = repo.MarkSubmitting(ctx, task.RequestID, 0, "submit-token-1")
	require.NoError(t, err)
	err = repo.MarkSubmitting(ctx, task.RequestID, 0, "stale-token")
	require.ErrorIs(t, err, service.ErrVideoTaskVersionConflict)

	nextPoll := time.Now().Add(time.Minute).UTC()
	err = repo.MarkSubmitted(ctx, service.MarkVideoSubmittedParams{
		RequestID:       task.RequestID,
		ExpectedVersion: 1,
		UpstreamTaskID:  "seedance-upstream-task-123",
		UpstreamStatus:  "queued",
		NextPollAt:      &nextPoll,
		SubmittedAt:     time.Now().UTC(),
	})
	require.NoError(t, err)

	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskSubmitted, stored.Status)
	require.Equal(t, int64(2), stored.Version)
	require.Equal(t, "seedance-upstream-task-123", videoTaskStringValue(stored.UpstreamTaskID))
	require.NotEqual(t, stored.RequestID, videoTaskStringValue(stored.UpstreamTaskID))
	require.Empty(t, stored.RequestPayload)
}

func TestVideoTaskRepositoryMarkSubmittedRetainsOnlyKlingRouteHint(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "kling-submit", "")
	params.Provider = service.VideoProviderKling
	task, _, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)

	require.NoError(t, repo.MarkSubmitting(ctx, task.RequestID, task.Version, "submit-token-kling"))
	pollPayload, err := service.NewMinimizedVideoPayload(map[string]any{
		"provider_task_kind": "image2video",
	})
	require.NoError(t, err)
	nextPoll := time.Now().Add(time.Minute).UTC()
	require.NoError(t, repo.MarkSubmitted(ctx, service.MarkVideoSubmittedParams{
		RequestID:       task.RequestID,
		ExpectedVersion: task.Version + 1,
		UpstreamTaskID:  "kling-task-accepted",
		UpstreamStatus:  "submitted",
		RequestPayload:  pollPayload,
		NextPollAt:      &nextPoll,
		SubmittedAt:     time.Now().UTC(),
	}))

	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.JSONEq(t, `{"provider_task_kind":"image2video"}`, string(stored.RequestPayload))
}

func TestVideoTaskRepositoryApplyPollResultUsesOptimisticVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "poll")

	leased, err := repo.LeaseDue(ctx, "poll-worker", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	leasedTask := findLeasedVideoTask(t, leased, task.RequestID)

	nextPoll := time.Now().Add(2 * time.Minute).UTC()
	_, err = repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID:       task.RequestID,
		ExpectedVersion: leasedTask.Version,
		LeaseOwner:      "poll-worker",
		Status:          service.VideoTaskRunning,
		UpstreamStatus:  "processing",
		NextPollAt:      &nextPoll,
	})
	require.NoError(t, err)

	_, err = repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID:       task.RequestID,
		ExpectedVersion: leasedTask.Version,
		LeaseOwner:      "poll-worker",
		Status:          service.VideoTaskSucceeded,
		UpstreamStatus:  "completed",
	})
	require.ErrorIs(t, err, service.ErrVideoTaskVersionConflict)
}

func TestVideoTaskRepositoryApplyPollResultRejectsExpiredOwnedLease(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "expired-poll")
	leased, err := repo.LeaseDue(ctx, "expired-worker", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	leasedTask := findLeasedVideoTask(t, leased, task.RequestID)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE video_tasks
		SET lease_expires_at = clock_timestamp() - INTERVAL '1 second'
		WHERE request_id = $1`, task.RequestID)
	require.NoError(t, err)

	_, err = repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID:       task.RequestID,
		ExpectedVersion: leasedTask.Version,
		LeaseOwner:      "expired-worker",
		Status:          service.VideoTaskRunning,
		UpstreamStatus:  "processing",
	})
	require.ErrorIs(t, err, service.ErrVideoTaskLeaseConflict)
}

func TestVideoTaskRepositoryRejectsMalformedRequestIDsAtEveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	taskError := service.NewVideoTaskError("UPSTREAM_ERROR", "safe message", true)
	eventPayload, err := service.NewMinimizedVideoPayload(map[string]any{"status": "created"})
	require.NoError(t, err)
	malformed := "vid_not-canonical"
	tests := map[string]func() error{
		"get owned":         func() error { _, err := repo.GetOwned(ctx, malformed, 1, 1); return err },
		"get by request id": func() error { _, err := repo.GetByRequestID(ctx, malformed); return err },
		"mark submitting":   func() error { return repo.MarkSubmitting(ctx, malformed, 0, "submit-token") },
		"mark submitted": func() error {
			return repo.MarkSubmitted(ctx, service.MarkVideoSubmittedParams{RequestID: malformed, ExpectedVersion: 0, UpstreamTaskID: "upstream"})
		},
		"mark submission unknown": func() error { return repo.MarkSubmissionUnknown(ctx, malformed, 0, taskError) },
		"apply poll result": func() error {
			_, err := repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{RequestID: malformed, LeaseOwner: "worker", Status: service.VideoTaskRunning})
			return err
		},
		"mark settled": func() error {
			return repo.MarkSettled(ctx, service.MarkVideoSettledParams{RequestID: malformed, BillingStatus: "settled"})
		},
		"release lease": func() error { return repo.ReleaseLease(ctx, malformed, "worker", time.Now()) },
		"append event": func() error {
			return repo.AppendEvent(ctx, service.VideoTaskEvent{RequestID: malformed, EventType: "created", Payload: eventPayload})
		},
		"list admin": func() error {
			_, _, err := repo.ListAdmin(ctx, service.VideoTaskListQuery{RequestID: malformed})
			return err
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, run(), service.ErrVideoTaskInvalidRequest)
		})
	}
}

func TestVideoTaskRepositoryDatabaseEnforcesPayloadAndNumericInvariants(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task, _, err := repo.CreateOrGet(ctx, videoTaskCreateParams(t, "database-invariants", ""))
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)

	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET upstream_task_id = 'direct-upstream' WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video_tasks_upstream_payload_cleared")
	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET frozen_amount = 'NaN'::numeric WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video_tasks_finite_amounts")
	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET settled_amount = 0.1, settled_at = clock_timestamp() WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video_tasks_settlement_status")
	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET status = 'succeeded', settled_amount = 0.1 WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video_tasks_settlement_complete")
	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET settled_at = clock_timestamp() WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video_tasks_settlement_complete")
}

func TestVideoTaskRepositoryRejectsNonFiniteAmountsAndPrematureSettlement(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	for name, mutate := range map[string]func(*service.CreateVideoTaskParams){
		"nan frozen amount":   func(params *service.CreateVideoTaskParams) { params.FrozenAmount = math.NaN() },
		"infinite unit price": func(params *service.CreateVideoTaskParams) { params.UnitPrice = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			params := videoTaskCreateParams(t, name, "")
			mutate(&params)
			_, _, err := repo.CreateOrGet(ctx, params)
			require.ErrorIs(t, err, service.ErrVideoTaskInvalidRequest)
		})
	}

	task, _, err := repo.CreateOrGet(ctx, videoTaskCreateParams(t, "premature-settle", ""))
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	err = repo.MarkSettled(ctx, service.MarkVideoSettledParams{
		RequestID:       task.RequestID,
		ExpectedVersion: task.Version,
		LeaseOwner:      "not-leased",
		SettledAmount:   0.1,
		BillingStatus:   "settled",
	})
	require.ErrorIs(t, err, service.ErrVideoTaskInvalidTransition)

	terminal := createDueVideoTask(t, repo, "terminal-settle")
	leased, err := repo.LeaseDue(ctx, "settlement-worker", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	leasedTask := findLeasedVideoTask(t, leased, terminal.RequestID)
	terminal, err = repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID:       terminal.RequestID,
		ExpectedVersion: leasedTask.Version,
		LeaseOwner:      "settlement-worker",
		Status:          service.VideoTaskSucceeded,
		UpstreamStatus:  "completed",
	})
	require.NoError(t, err)
	require.NoError(t, repo.MarkSettled(ctx, service.MarkVideoSettledParams{
		RequestID:       terminal.RequestID,
		ExpectedVersion: terminal.Version,
		LeaseOwner:      "settlement-worker",
		SettledAmount:   0.5,
		BillingStatus:   "settled",
	}))
	terminal, err = repo.GetByRequestID(ctx, terminal.RequestID)
	require.NoError(t, err)
	err = repo.MarkSettled(ctx, service.MarkVideoSettledParams{
		RequestID:       terminal.RequestID,
		ExpectedVersion: terminal.Version,
		LeaseOwner:      "settlement-worker",
		SettledAmount:   0.4,
		BillingStatus:   "settled",
	})
	require.ErrorIs(t, err, service.ErrVideoTaskInvalidTransition)
}

func TestVideoTaskRepositoryPersistsOnlyRedactedBoundedErrors(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task, _, err := repo.CreateOrGet(ctx, videoTaskCreateParams(t, "safe-errors", ""))
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	require.NoError(t, repo.MarkSubmitting(ctx, task.RequestID, task.Version, "submit-token"))
	taskError := service.NewVideoTaskError(
		"UPSTREAM_AUTH_FAILED",
		`{"x-api-key":"top-secret-key","authorization":"Bearer top-secret-token","message":"denied"}`,
		false,
	)
	require.NoError(t, repo.MarkSubmissionUnknown(ctx, task.RequestID, task.Version+1, taskError))

	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.NotNil(t, stored.LastErrorMessage)
	require.NotContains(t, *stored.LastErrorMessage, "top-secret")
	require.LessOrEqual(t, len(*stored.LastErrorMessage), service.MaxVideoTaskErrorMessageBytes)
}

func TestVideoTaskRepositoryAppendEventIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task, _, err := repo.CreateOrGet(ctx, videoTaskCreateParams(t, "events", ""))
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	payload, err := service.NewMinimizedVideoPayload(map[string]any{"status": "created"})
	require.NoError(t, err)

	require.NoError(t, repo.AppendEvent(ctx, service.VideoTaskEvent{
		RequestID: task.RequestID,
		EventType: "task_created",
		Payload:   payload,
	}))
	require.NoError(t, repo.AppendEvent(ctx, service.VideoTaskEvent{
		RequestID: task.RequestID,
		EventType: "task_observed",
		Payload:   payload,
	}))

	var count int
	err = integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_task_events WHERE request_id = $1`, task.RequestID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	_, err = integrationDB.ExecContext(ctx, `UPDATE video_task_events SET event_type = 'rewritten' WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video task events are append-only")
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM video_task_events WHERE request_id = $1`, task.RequestID)
	require.ErrorContains(t, err, "video task events are append-only")
}

func TestVideoTaskRepositoryLeaseDueTasksSkipsLockedRows(t *testing.T) {
	ctx := context.Background()
	repoA := NewVideoTaskRepository(integrationDB)
	repoB := NewVideoTaskRepository(integrationDB)
	first := createDueVideoTask(t, repoA, "lease-a")
	second := createDueVideoTask(t, repoA, "lease-b")
	installVideoTaskLeaseOverlapTrigger(t)
	type leaseResult struct {
		tasks []service.VideoTask
		err   error
	}
	results := make([]leaseResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, item := range []struct {
		repo  service.VideoTaskRepository
		owner string
	}{{repoA, "overlap-worker-a"}, {repoB, "overlap-worker-b"}} {
		wg.Add(1)
		go func(i int, repo service.VideoTaskRepository, owner string) {
			defer wg.Done()
			<-start
			results[i].tasks, results[i].err = repo.LeaseDue(ctx, owner, 1, time.Minute, time.Now().UTC())
		}(i, item.repo, item.owner)
	}
	close(start)
	wg.Wait()
	require.NoError(t, results[0].err)
	require.NoError(t, results[1].err)
	a := results[0].tasks
	b := results[1].tasks
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.NotEqual(t, a[0].RequestID, b[0].RequestID)
	require.ElementsMatch(t, []string{first.RequestID, second.RequestID}, []string{a[0].RequestID, b[0].RequestID})
}

func installVideoTaskLeaseOverlapTrigger(t *testing.T) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `
		CREATE OR REPLACE FUNCTION video_task_test_hold_lease()
		RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.lease_owner LIKE 'overlap-worker-%' AND OLD.lease_owner IS NULL THEN
				PERFORM pg_sleep(0.2);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_video_task_test_hold_lease
		BEFORE UPDATE ON video_tasks
		FOR EACH ROW EXECUTE FUNCTION video_task_test_hold_lease();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_video_task_test_hold_lease ON video_tasks`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS video_task_test_hold_lease()`)
	})
}

func TestVideoTaskRepositoryLeaseDueReclaimsExpiredLease(t *testing.T) {
	ctx := context.Background()
	repoA := NewVideoTaskRepository(integrationDB)
	repoB := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repoA, "expired-lease")
	now := time.Now().UTC()

	first, err := repoA.LeaseDue(ctx, "worker-a", 1, time.Minute, now)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, task.RequestID, first[0].RequestID)

	beforeExpiry, err := repoB.LeaseDue(ctx, "worker-b", 1, time.Minute, now.Add(30*time.Second))
	require.NoError(t, err)
	require.Empty(t, beforeExpiry)

	reclaimed, err := repoB.LeaseDue(ctx, "worker-b", 1, time.Minute, now.Add(61*time.Second))
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.Equal(t, task.RequestID, reclaimed[0].RequestID)
	require.Equal(t, "worker-b", videoTaskStringValue(reclaimed[0].LeaseOwner))
}

func videoTaskCreateParams(t *testing.T, label, idempotencyHash string) service.CreateVideoTaskParams {
	t.Helper()
	seed := videoTaskHash(fmt.Sprintf("%s-%s-%d", t.Name(), label, time.Now().UnixNano()))
	payload, err := service.NewMinimizedVideoPayload(map[string]any{
		"prompt":     "a safe prompt",
		"resolution": "720p",
	})
	require.NoError(t, err)
	return service.CreateVideoTaskParams{
		UserID:             videoTaskInt64(seed[0:12]),
		APIKeyID:           videoTaskInt64(seed[12:24]),
		GroupID:            videoTaskInt64(seed[24:36]),
		AccountID:          videoTaskInt64(seed[36:48]),
		Platform:           service.PlatformVideo,
		Provider:           service.VideoProviderSeedance,
		Operation:          "generation",
		ExternalModel:      "seedance-v1",
		UpstreamModel:      "seedance-1-0-pro",
		IdempotencyKeyHash: idempotencyHash,
		RequestHash:        videoTaskHash("request-" + seed),
		RequestPayload:     payload,
		PricingUnit:        "per_output_second",
		UnitPrice:          0.12,
		EstimatedUnits:     5,
		EstimatedAmount:    0.60,
		FrozenAmount:       0.60,
		Currency:           "USD",
		BillingMode:        "balance",
		BillingStatus:      "pending",
	}
}

func createDueVideoTask(t *testing.T, repo service.VideoTaskRepository, label string) *service.VideoTask {
	t.Helper()
	ctx := context.Background()
	task, _, err := repo.CreateOrGet(ctx, videoTaskCreateParams(t, label, ""))
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	require.NoError(t, repo.MarkSubmitting(ctx, task.RequestID, task.Version, "submit-token-"+label))
	due := time.Now().Add(-time.Minute).UTC()
	require.NoError(t, repo.MarkSubmitted(ctx, service.MarkVideoSubmittedParams{
		RequestID:       task.RequestID,
		ExpectedVersion: task.Version + 1,
		UpstreamTaskID:  "upstream-" + label + "-" + task.RequestID,
		UpstreamStatus:  "queued",
		NextPollAt:      &due,
		SubmittedAt:     time.Now().UTC(),
	}))
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	return stored
}

func findLeasedVideoTask(t *testing.T, tasks []service.VideoTask, requestID string) service.VideoTask {
	t.Helper()
	for _, task := range tasks {
		if task.RequestID == requestID {
			return task
		}
	}
	t.Fatalf("leased task %s not found in %d tasks", requestID, len(tasks))
	return service.VideoTask{}
}

func cleanupVideoTask(t *testing.T, requestID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM video_task_events WHERE request_id = $1`, requestID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM video_tasks WHERE request_id = $1`, requestID)
	})
}

func videoTaskHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func videoTaskInt64(value string) int64 {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	var out int64
	for _, b := range decoded {
		out = (out << 8) | int64(b)
	}
	if out == 0 {
		return 1
	}
	return out
}

func videoTaskStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
