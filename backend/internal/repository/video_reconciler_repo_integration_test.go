//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoReconcilerRepositoryTerminalUnsettledIsReLeasedAfterCrash(t *testing.T) {
	ctx := context.Background()
	repoA := NewVideoTaskRepository(integrationDB)
	repoB := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repoA, "terminal-release-after-crash")
	now := time.Now().UTC()
	leased, err := repoA.LeaseDue(ctx, "worker-terminal-a", 1, time.Minute, now)
	require.NoError(t, err)
	first := findLeasedVideoTask(t, leased, task.RequestID)
	terminalAt := now.Add(time.Second)
	terminal, err := repoA.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID: first.RequestID, ExpectedVersion: first.Version, LeaseOwner: "worker-terminal-a",
		Status: service.VideoTaskSucceeded, UpstreamStatus: "completed", NextPollAt: &terminalAt,
		ResultDurationSeconds: videoFloatPointer(6), UpdatedAt: terminalAt,
	})
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskSucceeded, terminal.Status)
	require.Equal(t, "worker-terminal-a", videoTaskStringValue(terminal.LeaseOwner), "terminal persistence must retain the live lease until settlement")

	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET lease_expires_at = clock_timestamp() - INTERVAL '1 second' WHERE request_id = $1`, task.RequestID)
	require.NoError(t, err)
	recovered, err := repoB.LeaseDue(ctx, "worker-terminal-b", 1, time.Minute, terminalAt.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	require.Equal(t, task.RequestID, recovered[0].RequestID)
	require.Equal(t, service.VideoTaskSucceeded, recovered[0].Status)
	require.Nil(t, recovered[0].SettledAt)
}

func TestVideoReconcilerRepositoryRenewsOnlyLiveOwnedLease(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "renew-live-owner")
	now := time.Now().UTC()
	leased, err := repo.LeaseDue(ctx, "worker-renew", 1, 2*time.Second, now)
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	originalExpiry := requireVideoTaskTime(t, active.LeaseExpiresAt)

	require.NoError(t, repo.RenewLease(ctx, service.RenewVideoTaskLeaseParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "worker-renew",
		LeaseDuration: 2 * time.Minute, UpdatedAt: now.Add(time.Second),
	}))
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.True(t, requireVideoTaskTime(t, stored.LeaseExpiresAt).After(originalExpiry.Add(time.Minute)))

	err = repo.RenewLease(ctx, service.RenewVideoTaskLeaseParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "wrong-worker",
		LeaseDuration: time.Minute, UpdatedAt: now.Add(2 * time.Second),
	})
	require.ErrorIs(t, err, service.ErrVideoTaskLeaseConflict)
}

func TestVideoReconcilerRepositoryLeaseClockUsesDatabaseAuthority(t *testing.T) {
	ctx := context.Background()
	repoA := NewVideoTaskRepository(integrationDB)
	repoB := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repoA, "database-lease-clock")

	var databaseNow time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow))
	dueBeforeSlowBusinessClock := databaseNow.Add(-48 * time.Hour)
	_, err := integrationDB.ExecContext(ctx, `UPDATE video_tasks SET next_poll_at = $2 WHERE request_id = $1`, task.RequestID, dueBeforeSlowBusinessClock)
	require.NoError(t, err)

	// The injected time controls only business due-ness. It must not shorten a
	// lease or make another replica believe the live lease already expired.
	slowBusinessClock := databaseNow.Add(-24 * time.Hour)
	leased, err := repoA.LeaseDue(ctx, "worker-db-clock-a", 1, 2*time.Minute, slowBusinessClock)
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	expiresAt := requireVideoTaskTime(t, active.LeaseExpiresAt)

	var databaseAfterLease time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseAfterLease))
	require.True(t, expiresAt.After(databaseAfterLease.Add(90*time.Second)), "lease expiry must be based on PostgreSQL clock, got %s after %s", expiresAt, databaseAfterLease)
	require.True(t, expiresAt.Before(databaseAfterLease.Add(150*time.Second)), "lease expiry must remain bounded by the requested duration")

	other, err := repoB.LeaseDue(ctx, "worker-db-clock-b", 1, time.Minute, databaseAfterLease)
	require.NoError(t, err)
	require.Empty(t, other, "a second repository must not steal a DB-live lease despite application clock skew")
}

func TestVideoReconcilerRepositoryTerminalSettlementRequiresLiveOwnerAndClearsLease(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "terminal-owner")
	leased, err := repo.LeaseDue(ctx, "worker-settle", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	terminal, err := repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "worker-settle",
		Status: service.VideoTaskFailed, Error: videoErrorPointer(service.NewVideoTaskError("UPSTREAM_TASK_FAILED", "", false)),
		NextPollAt: videoRepoTimePointer(time.Now().UTC()),
	})
	require.NoError(t, err)

	err = repo.MarkSettled(ctx, service.MarkVideoSettledParams{
		RequestID: terminal.RequestID, ExpectedVersion: terminal.Version, LeaseOwner: "wrong-worker",
		SettledAmount: 0, BillingStatus: "released", SettledAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, service.ErrVideoTaskLeaseConflict)
	require.NoError(t, repo.MarkSettled(ctx, service.MarkVideoSettledParams{
		RequestID: terminal.RequestID, ExpectedVersion: terminal.Version, LeaseOwner: "worker-settle",
		SettledAmount: 0, BillingStatus: "released", SettledAt: time.Now().UTC(),
	}))
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.NotNil(t, stored.SettledAt)
	require.Nil(t, stored.LeaseOwner)
	require.Nil(t, stored.LeaseExpiresAt)
	require.Nil(t, stored.NextPollAt)
}

func TestVideoReconcilerRepositoryFailedAndCancelledSettlementPreservesPollError(t *testing.T) {
	for _, status := range []service.VideoTaskStatus{service.VideoTaskFailed, service.VideoTaskCancelled} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			repo := NewVideoTaskRepository(integrationDB)
			task := createDueVideoTask(t, repo, "preserve-poll-error-"+string(status))
			owner := "worker-preserve-" + string(status)
			leased, err := repo.LeaseDue(ctx, owner, 1, time.Minute, time.Now().UTC())
			require.NoError(t, err)
			active := findLeasedVideoTask(t, leased, task.RequestID)
			pollError := service.NewVideoTaskError("CONTENT_REJECTED", "", false)
			terminal, err := repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
				RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: owner,
				Status: status, Error: &pollError, NextPollAt: videoRepoTimePointer(time.Now().UTC()),
			})
			require.NoError(t, err)
			require.NoError(t, repo.MarkSettled(ctx, service.MarkVideoSettledParams{
				RequestID: terminal.RequestID, ExpectedVersion: terminal.Version, LeaseOwner: owner,
				SettledAmount: 0, BillingStatus: "released", SettledAt: time.Now().UTC(),
			}))
			stored, err := repo.GetByRequestID(ctx, task.RequestID)
			require.NoError(t, err)
			require.Equal(t, "CONTENT_REJECTED", videoTaskStringValue(stored.LastErrorCode))
			require.False(t, stored.LastErrorRetryable)
		})
	}
}

func TestVideoReconcilerRepositoryRecoveryAndRetryHonorLeaseVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	due := time.Now().UTC().Add(-time.Minute)
	params := videoTaskCreateParams(t, "recover-owner", "")
	params.AccountID, params.UpstreamModel, params.NextPollAt = 0, "", &due
	task, _, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	require.NoError(t, repo.AssignAndMarkSubmitting(ctx, service.AssignVideoSubmissionParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version, AccountID: 77,
		Platform: service.PlatformVideo, Provider: service.VideoProviderSeedance,
		UpstreamModel: "seedance-recovery", ProviderSubmissionToken: "recover-token",
		NextPollAt: due, UpdatedAt: due,
	}))
	leased, err := repo.LeaseDue(ctx, "worker-recover", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)

	err = repo.ScheduleRetry(ctx, service.ScheduleVideoTaskRetryParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "wrong-worker",
		Status: service.VideoTaskUnknown, Error: service.NewVideoTaskError("RECOVERY_NOT_FOUND", "", true),
		NextPollAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, service.ErrVideoTaskLeaseConflict)
	require.NoError(t, repo.ScheduleRetry(ctx, service.ScheduleVideoTaskRetryParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "worker-recover",
		Status: service.VideoTaskUnknown, Error: service.NewVideoTaskError("RECOVERY_NOT_FOUND", "", true),
		NextPollAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC(),
	}))
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskUnknown, stored.Status)
	require.Nil(t, stored.LeaseOwner)
	require.Equal(t, "recover-token", videoTaskStringValue(stored.ProviderSubmissionToken))
	require.NotEmpty(t, stored.RequestPayload)
}

func TestVideoReconcilerRepositoryLeasesUnknownWithoutRecoveryTokenForReview(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	due := time.Now().UTC().Add(-time.Minute)
	params := videoTaskCreateParams(t, "unknown-manual-review", "")
	params.NextPollAt = &due
	task, _, err := repo.CreateOrGet(ctx, params)
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE video_tasks
		SET status = 'unknown', provider_submission_token = NULL, upstream_task_id = NULL,
		    next_poll_at = $2, version = version + 1
		WHERE request_id = $1`, task.RequestID, due)
	require.NoError(t, err)

	leased, err := repo.LeaseDue(ctx, "worker-unknown-review", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	require.Equal(t, service.VideoTaskUnknown, active.Status)
	require.Nil(t, active.ProviderSubmissionToken)
	require.Nil(t, active.UpstreamTaskID)
}

func TestVideoReconcilerRepositoryPollExhaustionPreservesAcceptedRouteAndHoldForReview(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "poll-exhaustion-review")
	leased, err := repo.LeaseDue(ctx, "worker-poll-review", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	nextReview := time.Now().UTC().Add(24 * time.Hour)
	require.NoError(t, repo.ScheduleRetry(ctx, service.ScheduleVideoTaskRetryParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "worker-poll-review",
		Status: service.VideoTaskUnknown, Error: service.NewVideoTaskError("POLL_ATTEMPTS_EXHAUSTED", "", false),
		NextPollAt: nextReview, IncrementPollAttempts: true, UpdatedAt: time.Now().UTC(),
	}))
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, service.VideoTaskUnknown, stored.Status)
	require.Equal(t, active.AccountID, stored.AccountID)
	require.Equal(t, active.Provider, stored.Provider)
	require.Equal(t, videoTaskStringValue(active.UpstreamTaskID), videoTaskStringValue(stored.UpstreamTaskID))
	require.Equal(t, active.FrozenAmount, stored.FrozenAmount)
	require.Nil(t, stored.SettledAt)
	require.Nil(t, stored.SettledAmount)
	require.Equal(t, "held", stored.BillingStatus)
	require.Equal(t, "POLL_ATTEMPTS_EXHAUSTED", videoTaskStringValue(stored.LastErrorCode))
}

func TestVideoReconcilerRepositoryBillingReplayAfterCrashIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	billingRepo := NewUsageBillingRepository(client, integrationDB)
	billing := service.NewVideoBillingService(billingRepo)
	task, _, user := createVideoBillingTask(t, client, repo, "settlement-replay", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billingRepo, task))
	require.NoError(t, repo.MarkSubmitting(ctx, task.RequestID, task.Version, "submit-token"))
	due := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, repo.MarkSubmitted(ctx, service.MarkVideoSubmittedParams{
		RequestID: task.RequestID, ExpectedVersion: task.Version + 1, UpstreamTaskID: "upstream-settlement-replay",
		UpstreamStatus: "queued", NextPollAt: &due, SubmittedAt: due,
	}))
	leased, err := repo.LeaseDue(ctx, "worker-billing-replay", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	terminal, err := repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "worker-billing-replay",
		Status: service.VideoTaskSucceeded, ResultDurationSeconds: videoFloatPointer(5), NextPollAt: videoRepoTimePointer(time.Now().UTC()),
	})
	require.NoError(t, err)
	require.NoError(t, billing.Capture(ctx, *terminal, 1.25))
	// Simulate a process crash after the durable capture but before MarkSettled.
	require.NoError(t, billing.Capture(ctx, *terminal, 1.25), "same stable capture operation must replay as success")
	require.NoError(t, repo.MarkSettled(ctx, service.MarkVideoSettledParams{
		RequestID: terminal.RequestID, ExpectedVersion: terminal.Version, LeaseOwner: "worker-billing-replay",
		SettledAmount: 1.25, BillingStatus: "captured", BillingReference: videoRepoStringPointer(service.VideoCaptureRequestID(task.RequestID)), SettledAt: time.Now().UTC(),
	}))
	assertVideoBalanceExact(t, user.ID, "8.75", "0")
}

func TestVideoReconcilerRepositoryRetentionClearsOnlyExpiringMetadata(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "retention-safe")
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE video_tasks
		SET status = 'succeeded', upstream_task_id = NULL,
		    result_url = 'https://cdn.example.com/expired.mp4',
		    result_url_expires_at = $2, result_content_type = 'video/mp4',
		    result_duration_seconds = 5, result_width = 1280, result_height = 720,
		    finished_at = $2, request_payload = '{"provider_task_kind":"text2video"}'::jsonb,
		    settled_amount = 0.5, settled_at = $2, billing_status = 'captured', billing_reference = 'video_capture:test',
		    next_poll_at = NULL, lease_owner = NULL, lease_expires_at = NULL
		WHERE request_id = $1`, task.RequestID, old)
	require.NoError(t, err)
	payload, err := service.NewMinimizedVideoPayload(map[string]any{"status": "succeeded"})
	require.NoError(t, err)
	require.NoError(t, repo.AppendEvent(ctx, service.VideoTaskEvent{RequestID: task.RequestID, EventType: "terminal", Payload: payload, CreatedAt: old}))

	cleared, err := repo.ClearExpiredMetadata(ctx, service.ClearVideoTaskMetadataParams{Now: time.Now().UTC(), RetentionBefore: time.Now().UTC().Add(-30 * 24 * time.Hour), Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, cleared)
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Nil(t, stored.ResultURL)
	require.Nil(t, stored.ResultURLExpiresAt)
	require.Nil(t, stored.ResultContentType)
	require.Nil(t, stored.ResultDurationSeconds)
	require.Nil(t, stored.ResultWidth)
	require.Nil(t, stored.ResultHeight)
	require.Empty(t, stored.RequestPayload)
	require.NotNil(t, stored.SettledAmount)
	require.NotNil(t, stored.SettledAt)
	require.NotNil(t, stored.BillingReference)
	var taskCount, eventCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tasks WHERE request_id = $1`, task.RequestID).Scan(&taskCount))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_task_events WHERE request_id = $1`, task.RequestID).Scan(&eventCount))
	require.Equal(t, 1, taskCount)
	require.Equal(t, 1, eventCount)
}

func TestVideoReconcilerRepositoryRetentionDoesNotRaceUnsettledTerminalWorker(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "retention-unsettled")
	now := time.Now().UTC()
	leased, err := repo.LeaseDue(ctx, "worker-retention-live", 1, time.Minute, now)
	require.NoError(t, err)
	active := findLeasedVideoTask(t, leased, task.RequestID)
	expired := now.Add(-time.Hour)
	terminal, err := repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID: active.RequestID, ExpectedVersion: active.Version, LeaseOwner: "worker-retention-live",
		Status: service.VideoTaskSucceeded, ResultURL: videoRepoStringPointer("https://cdn.example.com/still-settling.mp4"),
		ResultURLExpiresAt: &expired, ResultDurationSeconds: videoFloatPointer(5), NextPollAt: &now, UpdatedAt: now,
	})
	require.NoError(t, err)

	cleared, err := repo.ClearExpiredMetadata(ctx, service.ClearVideoTaskMetadataParams{
		Now: now, RetentionBefore: now.Add(-30 * 24 * time.Hour), Limit: 10,
	})
	require.NoError(t, err)
	require.Zero(t, cleared)
	stored, err := repo.GetByRequestID(ctx, task.RequestID)
	require.NoError(t, err)
	require.Equal(t, terminal.Version, stored.Version)
	require.NotNil(t, stored.ResultURL)
	require.Nil(t, stored.SettledAt)
}

func videoFloatPointer(value float64) *float64                               { return &value }
func videoErrorPointer(value service.VideoTaskError) *service.VideoTaskError { return &value }
func videoRepoTimePointer(value time.Time) *time.Time                        { return &value }
func videoRepoStringPointer(value string) *string                            { return &value }

func requireVideoTaskTime(t *testing.T, value *time.Time) time.Time {
	t.Helper()
	require.NotNil(t, value)
	return *value
}
