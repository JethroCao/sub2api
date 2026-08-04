//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
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

func TestVideoTaskRepositoryCreateIdempotencyConflict(t *testing.T) {
	repoA := NewVideoTaskRepository(integrationDB)
	repoB := NewVideoTaskRepository(integrationDB)
	params := videoTaskCreateParams(t, "idempotency", videoTaskHash("same-key"+t.Name()))

	first, created, err := repoA.CreateOrGet(context.Background(), params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, first.RequestID)

	replayed, created, err := repoB.CreateOrGet(context.Background(), params)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.RequestID, replayed.RequestID)

	params.RequestHash = videoTaskHash("different-request")
	_, _, err = repoB.CreateOrGet(context.Background(), params)
	require.ErrorIs(t, err, service.ErrVideoIdempotencyConflict)
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

func TestVideoTaskRepositoryApplyPollResultUsesOptimisticVersion(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	task := createDueVideoTask(t, repo, "poll")

	leased, err := repo.LeaseDue(ctx, "poll-worker", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	leasedTask := findLeasedVideoTask(t, leased, task.RequestID)

	nextPoll := time.Now().Add(2 * time.Minute).UTC()
	err = repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID:       task.RequestID,
		ExpectedVersion: leasedTask.Version,
		LeaseOwner:      "poll-worker",
		Status:          service.VideoTaskRunning,
		UpstreamStatus:  "processing",
		NextPollAt:      &nextPoll,
	})
	require.NoError(t, err)

	err = repo.ApplyPollResult(ctx, service.ApplyVideoPollResultParams{
		RequestID:       task.RequestID,
		ExpectedVersion: leasedTask.Version,
		LeaseOwner:      "poll-worker",
		Status:          service.VideoTaskSucceeded,
		UpstreamStatus:  "completed",
	})
	require.ErrorIs(t, err, service.ErrVideoTaskVersionConflict)
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

	a, err := repoA.LeaseDue(ctx, "worker-a", 1, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	b, err := repoB.LeaseDue(ctx, "worker-b", 2, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.NotEqual(t, a[0].RequestID, b[0].RequestID)
	require.ElementsMatch(t, []string{first.RequestID, second.RequestID}, []string{a[0].RequestID, b[0].RequestID})
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
