//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUsageBillingRepositoryVideoHoldPrecisionBoundaryLifecycle(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)

	t.Run("below half quantum becomes a zero hold", func(t *testing.T) {
		task, _, user := createVideoBillingTask(t, client, taskRepo, "precision-below-half", "balance", nil, 0.000000004, 0.00000001, nil, 0)
		var storedIsZero bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT frozen_amount = 0 FROM video_tasks WHERE request_id = $1`, task.RequestID).Scan(&storedIsZero))
		require.True(t, storedIsZero)
		_, err := billing.ReserveVideo(ctx, videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0))
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0.00000001", "0")
	})

	t.Run("half quantum is stored and moved as one exact ledger unit", func(t *testing.T) {
		task, _, user := createVideoBillingTask(t, client, taskRepo, "precision-half-unit", "balance", nil, 0.000000005, 0.00000001, nil, 0)
		var storedIsOneUnit bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT frozen_amount = 0.00000001 FROM video_tasks WHERE request_id = $1`, task.RequestID).Scan(&storedIsOneUnit))
		require.True(t, storedIsOneUnit)
		_, err := billing.ReserveVideo(ctx, videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0))
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0", "0.00000001")
		_, err = billing.ReleaseVideo(ctx, videoHoldCommand(task, service.VideoReleaseRequestID(task.RequestID), 0))
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0.00000001", "0")
	})

	t.Run("capture rounds actual once and enforces the canonical cap", func(t *testing.T) {
		task, _, user := createVideoBillingTask(t, client, taskRepo, "precision-capture", "balance", nil, 0.000000005, 0.00000002, nil, 0)
		require.NoError(t, reserveVideoTask(t, billing, task))
		_, err := billing.CaptureVideo(ctx, videoHoldCommand(task, service.VideoCaptureRequestID(task.RequestID), 0.000000005))
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0.00000001", "0")

		over, _, _ := createVideoBillingTask(t, client, taskRepo, "precision-capture-over", "balance", nil, 0.000000005, 0.00000002, nil, 0)
		require.NoError(t, reserveVideoTask(t, billing, over))
		_, err = billing.CaptureVideo(ctx, videoHoldCommand(over, service.VideoCaptureRequestID(over.RequestID), 0.000000015))
		require.ErrorIs(t, err, service.ErrVideoFinalCostExceedsHold)
	})

	t.Run("large exact numeric never round trips through float", func(t *testing.T) {
		task, _, user := createVideoBillingTask(t, client, taskRepo, "precision-large", "balance", nil, 1, 1, nil, 0)
		const exact = "9999999999.12345679"
		_, err := integrationDB.ExecContext(ctx, `UPDATE users SET balance = $2::numeric, frozen_balance = 0 WHERE id = $1`, user.ID, exact)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET frozen_amount = $2::numeric WHERE request_id = $1`, task.RequestID, exact)
		require.NoError(t, err)

		_, err = billing.ReserveVideo(ctx, videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0))
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0", exact)
		_, err = billing.ReleaseVideo(ctx, videoHoldCommand(task, service.VideoReleaseRequestID(task.RequestID), 0))
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, exact, "0")
	})
}

func TestUsageBillingRepositoryBatchImageRetainsScaleEightPrecisionBehavior(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	user := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("batch-precision-%d@example.com", time.Now().UnixNano()), PasswordHash: "hash"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-batch-precision-" + uuid.NewString(), Name: "batch-precision"})

	t.Run("subquantum balance remains insufficient", func(t *testing.T) {
		_, err := integrationDB.ExecContext(ctx, `UPDATE users SET balance = 0.000000004, frozen_balance = 0 WHERE id = $1`, user.ID)
		require.NoError(t, err)
		batchID := "imgbatch_precision_insufficient_" + uuid.NewString()
		_, err = billing.ReserveBatchImageBalance(ctx, &service.BatchImageBalanceHoldCommand{
			RequestID: service.BatchImageHoldRequestID(batchID), APIKeyID: apiKey.ID,
			UserID: user.ID, BatchID: batchID, HoldAmount: 0.000000004,
		})
		require.ErrorIs(t, err, service.ErrBatchImageInsufficientBalance)
		assertVideoBalanceExact(t, user.ID, "0", "0")
	})

	t.Run("half quantum lifecycle keeps legacy rounding", func(t *testing.T) {
		_, err := integrationDB.ExecContext(ctx, `UPDATE users SET balance = 0.00000001, frozen_balance = 0 WHERE id = $1`, user.ID)
		require.NoError(t, err)
		batchID := "imgbatch_precision_release_" + uuid.NewString()
		reserve := &service.BatchImageBalanceHoldCommand{
			RequestID: service.BatchImageHoldRequestID(batchID), APIKeyID: apiKey.ID,
			UserID: user.ID, BatchID: batchID, HoldAmount: 0.000000005,
		}
		_, err = billing.ReserveBatchImageBalance(ctx, reserve)
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0.00000001", "0.00000001")
		_, err = billing.ReleaseBatchImageBalance(ctx, &service.BatchImageBalanceHoldCommand{
			RequestID: service.BatchImageReleaseRequestID(batchID), APIKeyID: apiKey.ID,
			UserID: user.ID, BatchID: batchID, HoldAmount: 0.000000005,
		})
		require.NoError(t, err)
		assertVideoBalanceExact(t, user.ID, "0.00000002", "0.00000001")
	})
}

func TestVideoTaskRepositoryMarkSettledUsesCanonicalNumericCap(t *testing.T) {
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)

	t.Run("accepts floating noise that canonicalizes to the hold", func(t *testing.T) {
		task := createTerminalVideoTaskWithExactHold(t, repo, "settle-noise", "0.30000000")
		left, right := 0.1, 0.2
		noisy := left + right
		require.Greater(t, noisy, 0.3)
		err := repo.MarkSettled(ctx, service.MarkVideoSettledParams{
			RequestID: task.RequestID, ExpectedVersion: task.Version,
			LeaseOwner: videoTaskStringValue(task.LeaseOwner), SettledAmount: noisy, BillingStatus: "settled",
		})
		require.NoError(t, err)
		var canonical bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT settled_amount = 0.30000000 FROM video_tasks WHERE request_id = $1`, task.RequestID).Scan(&canonical))
		require.True(t, canonical)
	})

	t.Run("rejects an amount that rounds one unit above the hold", func(t *testing.T) {
		task := createTerminalVideoTaskWithExactHold(t, repo, "settle-rounded-over", "0.30000000")
		err := repo.MarkSettled(ctx, service.MarkVideoSettledParams{
			RequestID: task.RequestID, ExpectedVersion: task.Version,
			LeaseOwner: videoTaskStringValue(task.LeaseOwner), SettledAmount: 0.300000005, BillingStatus: "settled",
		})
		require.ErrorIs(t, err, service.ErrVideoFinalCostExceedsHold)
		assertVideoTaskSettlementUnchanged(t, task.RequestID, task.Version)
	})

	t.Run("rejects large float conversion above the exact hold", func(t *testing.T) {
		task := createTerminalVideoTaskWithExactHold(t, repo, "settle-large-over", "9999999999.12345678")
		var floatRoundTrip float64
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT frozen_amount FROM video_tasks WHERE request_id = $1`, task.RequestID).Scan(&floatRoundTrip))
		err := repo.MarkSettled(ctx, service.MarkVideoSettledParams{
			RequestID: task.RequestID, ExpectedVersion: task.Version,
			LeaseOwner: videoTaskStringValue(task.LeaseOwner), SettledAmount: floatRoundTrip, BillingStatus: "settled",
		})
		require.ErrorIs(t, err, service.ErrVideoFinalCostExceedsHold)
		assertVideoTaskSettlementUnchanged(t, task.RequestID, task.Version)
	})
}

func createTerminalVideoTaskWithExactHold(t *testing.T, repo service.VideoTaskRepository, label, hold string) *service.VideoTask {
	t.Helper()
	task, _, err := repo.CreateOrGet(context.Background(), videoTaskCreateParams(t, label, ""))
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	_, err = integrationDB.ExecContext(context.Background(), `
		UPDATE video_tasks SET status = 'succeeded', frozen_amount = $2::numeric,
		       next_poll_at = clock_timestamp() WHERE request_id = $1
	`, task.RequestID, hold)
	require.NoError(t, err)
	owner := "worker-settle-" + label
	leased, err := repo.LeaseDue(context.Background(), owner, 1, time.Minute, time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	leasedTask := findLeasedVideoTask(t, leased, task.RequestID)
	return &leasedTask
}

func assertVideoTaskSettlementUnchanged(t *testing.T, requestID string, version int64) {
	t.Helper()
	var unchanged bool
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT settled_amount IS NULL AND settled_at IS NULL AND version = $2
		FROM video_tasks WHERE request_id = $1
	`, requestID, version).Scan(&unchanged))
	require.True(t, unchanged)
}

func assertVideoBalanceExact(t *testing.T, userID int64, wantBalance, wantFrozen string) {
	t.Helper()
	var balanceMatches, frozenMatches bool
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT balance = $2::numeric, frozen_balance = $3::numeric FROM users WHERE id = $1
	`, userID, wantBalance, wantFrozen).Scan(&balanceMatches, &frozenMatches))
	require.True(t, balanceMatches, "balance must equal %s exactly", wantBalance)
	require.True(t, frozenMatches, "frozen balance must equal %s exactly", wantFrozen)
}

func TestUsageBillingRepositoryRejectsNonCanonicalVideoOperationKeys(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)

	tests := []struct {
		name      string
		prepare   func(*testing.T, *service.VideoTask)
		mutate    func(context.Context, *service.VideoHoldCommand) (*service.VideoHoldResult, error)
		canonical func(string) string
	}{
		{name: "reserve", mutate: billing.ReserveVideo, canonical: service.VideoHoldRequestID},
		{name: "capture", prepare: func(t *testing.T, task *service.VideoTask) { require.NoError(t, reserveVideoTask(t, billing, task)) }, mutate: billing.CaptureVideo, canonical: service.VideoCaptureRequestID},
		{name: "release", prepare: func(t *testing.T, task *service.VideoTask) { require.NoError(t, reserveVideoTask(t, billing, task)) }, mutate: billing.ReleaseVideo, canonical: service.VideoReleaseRequestID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task, apiKey, user := createVideoBillingTask(t, client, taskRepo, "wrong-key-"+tt.name, "balance", nil, 2, 10, nil, 0)
			if tt.prepare != nil {
				tt.prepare(t, task)
			}
			var beforeBalance, beforeFrozen float64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance, frozen_balance FROM users WHERE id = $1`, user.ID).Scan(&beforeBalance, &beforeFrozen))
			cmd := videoHoldCommand(task, "custom:"+uuid.NewString(), 1)
			_, err := tt.mutate(ctx, cmd)
			require.ErrorIs(t, err, service.ErrVideoTaskInvalidRequest)
			var afterBalance, afterFrozen float64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance, frozen_balance FROM users WHERE id = $1`, user.ID).Scan(&afterBalance, &afterFrozen))
			require.Equal(t, beforeBalance, afterBalance)
			require.Equal(t, beforeFrozen, afterFrozen)
			var claimCount int
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2`, cmd.RequestID, apiKey.ID).Scan(&claimCount))
			require.Zero(t, claimCount)
			require.NotEqual(t, tt.canonical(task.RequestID), cmd.RequestID)
		})
	}
}

func TestUsageBillingRepositoryRepeatedVideoHoldRequiresCanonicalLiveOrArchiveKey(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	for _, archived := range []bool{false, true} {
		name := "live"
		if archived {
			name = "archive"
		}
		t.Run(name, func(t *testing.T) {
			task, apiKey, user := createVideoBillingTask(t, client, taskRepo, "repeat-key-"+name, "balance", nil, 2, 10, nil, 0)
			require.NoError(t, reserveVideoTask(t, billing, task))
			canonical := service.VideoHoldRequestID(task.RequestID)
			if archived {
				_, err := integrationDB.ExecContext(ctx, `
					INSERT INTO usage_billing_dedup_archive (request_id, api_key_id, request_fingerprint, created_at)
					SELECT request_id, api_key_id, request_fingerprint, created_at
					FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2
					ON CONFLICT (request_id, api_key_id) DO NOTHING
				`, canonical, apiKey.ID)
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, `DELETE FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2`, canonical, apiKey.ID)
				require.NoError(t, err)
			}
			cmd := videoHoldCommand(task, "video_hold:wrong:"+uuid.NewString(), 0)
			_, err := billing.ReserveVideo(ctx, cmd)
			require.ErrorIs(t, err, service.ErrVideoTaskInvalidRequest)
			assertVideoBalanceExact(t, user.ID, "8", "2")
		})
	}
}

func TestVideoTaskRepositoryBillingMetadataRequiresOwnedSubscription(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewVideoTaskRepository(integrationDB)
	baseTask, _, _ := createVideoBillingTask(t, client, repo, "metadata-owner", "balance", nil, 1, 10, nil, 0)
	owned := mustCreateSubscription(t, client, &service.UserSubscription{UserID: baseTask.UserID, GroupID: baseTask.GroupID})
	otherUser := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("video-metadata-other-%d@example.com", time.Now().UnixNano()), PasswordHash: "hash"})
	foreign := mustCreateSubscription(t, client, &service.UserSubscription{UserID: otherUser.ID, GroupID: baseTask.GroupID})

	newParams := func(label string) service.CreateVideoTaskParams {
		params := videoTaskCreateParams(t, label, "")
		params.UserID = baseTask.UserID
		params.APIKeyID = baseTask.APIKeyID
		params.GroupID = baseTask.GroupID
		params.AccountID = baseTask.AccountID
		return params
	}

	t.Run("normalizes valid balance", func(t *testing.T) {
		params := newParams("metadata-balance")
		params.BillingMode = "  BALANCE "
		task, _, err := repo.CreateOrGet(ctx, params)
		require.NoError(t, err)
		require.Equal(t, "balance", task.BillingMode)
		cleanupVideoTask(t, task.RequestID)
	})
	t.Run("rejects balance with subscription", func(t *testing.T) {
		params := newParams("metadata-mixed")
		params.SubscriptionID = &owned.ID
		_, _, err := repo.CreateOrGet(ctx, params)
		require.ErrorIs(t, err, service.ErrVideoTaskInvalidRequest)
	})
	t.Run("rejects foreign subscription", func(t *testing.T) {
		params := newParams("metadata-foreign")
		params.BillingMode = "subscription"
		params.SubscriptionID = &foreign.ID
		_, _, err := repo.CreateOrGet(ctx, params)
		require.ErrorIs(t, err, service.ErrVideoTaskInvalidRequest)
	})
	t.Run("accepts owned subscription", func(t *testing.T) {
		params := newParams("metadata-owned")
		params.BillingMode = " SUBSCRIPTION "
		params.SubscriptionID = &owned.ID
		task, _, err := repo.CreateOrGet(ctx, params)
		require.NoError(t, err)
		require.Equal(t, "subscription", task.BillingMode)
		require.Equal(t, owned.ID, *task.SubscriptionID)
		cleanupVideoTask(t, task.RequestID)
	})
}

func TestCreateVideoTaskAndReserveConcurrentReplayOfUnheldTaskDoesNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("video-replay-lock-%d@example.com", time.Now().UnixNano()), PasswordHash: "hash", Balance: 10})
	group := mustCreateGroup(t, client, &service.Group{Name: "video-replay-lock-" + uuid.NewString(), Platform: service.PlatformVideo, SubscriptionType: service.SubscriptionTypeStandard, AllowVideoGeneration: true})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-video-replay-lock-" + uuid.NewString(), Name: "video-replay-lock"})
	account := mustCreateAccount(t, client, &service.Account{Name: "video-replay-lock-" + uuid.NewString(), Platform: service.PlatformVideo})
	params := videoTaskCreateParams(t, "concurrent-unheld-replay", videoTaskHash("concurrent-unheld-replay-"+t.Name()))
	params.UserID, params.APIKeyID, params.GroupID, params.AccountID = user.ID, apiKey.ID, group.ID, account.ID
	params.FrozenAmount, params.EstimatedAmount = 2, 2
	preexisting, created, err := NewVideoTaskRepository(integrationDB).CreateOrGet(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, preexisting.RequestID)
	installVideoHoldClaimOverlapTrigger(t, apiKey.ID)

	type result struct {
		task *service.VideoTask
		err  error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].task, _, results[i].err = NewVideoTaskRepository(integrationDB).CreateTaskAndReserve(ctx, params)
		}(i)
	}
	close(start)
	wg.Wait()
	for _, result := range results {
		require.NoError(t, result.err)
		require.NotNil(t, result.task)
		require.Equal(t, preexisting.RequestID, result.task.RequestID)
	}
	assertVideoBalanceExact(t, user.ID, "8", "2")
}

func TestStandaloneReserveAndAtomicReplayUseOneTaskBeforeClaimLockOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	user := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("video-cross-entry-lock-%d@example.com", time.Now().UnixNano()), PasswordHash: "hash", Balance: 10})
	group := mustCreateGroup(t, client, &service.Group{Name: "video-cross-entry-lock-" + uuid.NewString(), Platform: service.PlatformVideo, SubscriptionType: service.SubscriptionTypeStandard, AllowVideoGeneration: true})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-video-cross-entry-lock-" + uuid.NewString(), Name: "video-cross-entry-lock"})
	account := mustCreateAccount(t, client, &service.Account{Name: "video-cross-entry-lock-" + uuid.NewString(), Platform: service.PlatformVideo})
	params := videoTaskCreateParams(t, "cross-entry-unheld-replay", videoTaskHash("cross-entry-unheld-replay-"+t.Name()))
	params.UserID, params.APIKeyID, params.GroupID, params.AccountID = user.ID, apiKey.ID, group.ID, account.ID
	params.FrozenAmount, params.EstimatedAmount = 2, 2
	preexisting, created, err := NewVideoTaskRepository(integrationDB).CreateOrGet(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, preexisting.RequestID)
	installVideoHoldClaimAdvisoryPauseTrigger(t, apiKey.ID)

	standaloneErr := make(chan error, 1)
	go func() {
		_, reserveErr := billing.ReserveVideo(ctx, videoHoldCommand(preexisting, service.VideoHoldRequestID(preexisting.RequestID), 0))
		standaloneErr <- reserveErr
	}()
	waitForVideoHoldClaimAdvisoryLock(t, ctx)

	atomicErr := make(chan error, 1)
	go func() {
		_, _, reserveErr := NewVideoTaskRepository(integrationDB).CreateTaskAndReserve(ctx, params)
		atomicErr <- reserveErr
	}()
	require.NoError(t, <-standaloneErr)
	require.NoError(t, <-atomicErr)
	assertVideoBalanceExact(t, user.ID, "8", "2")
}

const (
	videoHoldTestAdvisoryClass = 42
	videoHoldTestAdvisoryKey   = 610062
)

func installVideoHoldClaimAdvisoryPauseTrigger(t *testing.T, apiKeyID int64) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION video_hold_claim_advisory_pause_test()
		RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.api_key_id = %d AND NEW.request_id LIKE 'video_hold:%%' THEN
				PERFORM pg_advisory_xact_lock(%d, %d);
				PERFORM pg_sleep(1.25);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_video_hold_claim_advisory_pause_test
		BEFORE INSERT ON usage_billing_dedup
		FOR EACH ROW EXECUTE FUNCTION video_hold_claim_advisory_pause_test();
	`, apiKeyID, videoHoldTestAdvisoryClass, videoHoldTestAdvisoryKey))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_video_hold_claim_advisory_pause_test ON usage_billing_dedup`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS video_hold_claim_advisory_pause_test()`)
	})
}

func waitForVideoHoldClaimAdvisoryLock(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var held bool
		err := integrationDB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND granted
			)
		`, videoHoldTestAdvisoryClass, videoHoldTestAdvisoryKey).Scan(&held)
		require.NoError(t, err)
		if held {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("standalone reserve did not enter the billing-claim trigger")
}

func installVideoHoldClaimOverlapTrigger(t *testing.T, apiKeyID int64) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION video_hold_claim_overlap_test()
		RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.api_key_id = %d AND NEW.request_id LIKE 'video_hold:%%' THEN
				PERFORM pg_sleep(0.75);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_video_hold_claim_overlap_test
		BEFORE INSERT ON usage_billing_dedup
		FOR EACH ROW EXECUTE FUNCTION video_hold_claim_overlap_test();
	`, apiKeyID))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_video_hold_claim_overlap_test ON usage_billing_dedup`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS video_hold_claim_overlap_test()`)
	})
}
