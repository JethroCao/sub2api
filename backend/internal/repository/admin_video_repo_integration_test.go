//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	migrationfiles "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestAdminVideoPricingReplaceIsAtomicForConcurrentReaders(t *testing.T) {
	ctx := context.Background()
	groupID := createVideoPricingTestGroup(t)
	defer deleteVideoPricingTestGroup(t, groupID)
	createVideoPricingTestRule(t, groupID, "seedance-2.0", "generation", "*", "any", "per_request", 1, true)

	repo := NewAdminVideoRepository(integrationDB)
	_, err := integrationDB.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION admin_video_test_pause_replace()
		RETURNS TRIGGER AS $$ BEGIN PERFORM pg_sleep(0.6); RETURN NEW; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_admin_video_test_pause_replace
		BEFORE INSERT ON video_pricing_rules
		FOR EACH ROW WHEN (NEW.unit_price = 2) EXECUTE FUNCTION admin_video_test_pause_replace();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_admin_video_test_pause_replace ON video_pricing_rules`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS admin_video_test_pause_replace()`)
	})

	errCh := make(chan error, 1)
	go func() {
		_, replaceErr := repo.ReplacePricingRules(ctx, groupID, []service.VideoPricingRuleInput{{
			ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any",
			Unit: "per_request", UnitPrice: 2, Enabled: true,
		}})
		errCh <- replaceErr
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rules, listErr := repo.ListPricingRules(ctx, groupID)
		require.NoError(t, listErr)
		require.Len(t, rules, 1, "reader must observe the old set or the committed replacement, never the delete/insert gap")
		require.Contains(t, []float64{1, 2}, rules[0].UnitPrice)
		select {
		case replaceErr := <-errCh:
			require.NoError(t, replaceErr)
			return
		default:
		}
	}
	t.Fatal("pricing replacement did not complete")
}

func TestAdminVideoRefundIsAtomicAndIdempotentAcrossKeys(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	taskRepo := NewVideoTaskRepository(integrationDB)
	billing := NewUsageBillingRepository(client, integrationDB)
	task, _, user := createVideoBillingTask(t, client, taskRepo, "admin-refund", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))
	_, err := integrationDB.ExecContext(ctx, `UPDATE video_tasks SET status='unknown', billing_status='held' WHERE request_id=$1`, task.RequestID)
	require.NoError(t, err)

	repo := NewAdminVideoRepository(integrationDB)
	commands := []service.AdminVideoRefundMutation{
		{RequestID: task.RequestID, AdminVideoActionMetadata: service.AdminVideoActionMetadata{ActorUserID: 7, Reason: "provider confirmed absent", IdempotencyKeyHash: service.HashIdempotencyKey("key-a"), RequestHash: service.HashIdempotencyKey("payload-a")}},
		{RequestID: task.RequestID, AdminVideoActionMetadata: service.AdminVideoActionMetadata{ActorUserID: 7, Reason: "provider confirmed absent", IdempotencyKeyHash: service.HashIdempotencyKey("key-b"), RequestHash: service.HashIdempotencyKey("payload-b")}},
	}
	var wg sync.WaitGroup
	results := make(chan error, len(commands))
	for _, command := range commands {
		command := command
		wg.Add(1)
		go func() { defer wg.Done(); _, actionErr := repo.Refund(ctx, command); results <- actionErr }()
	}
	wg.Wait()
	close(results)
	for actionErr := range results {
		if actionErr != nil {
			require.ErrorIs(t, actionErr, service.ErrVideoFinancialStateConflict)
		}
	}

	assertVideoBalanceExact(t, user.ID, "10", "0")
	var releaseClaims, events, audits int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id=$1`, service.VideoReleaseRequestID(task.RequestID)).Scan(&releaseClaims))
	require.Equal(t, 1, releaseClaims)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_task_events WHERE request_id=$1 AND event_type='admin_refund'`, task.RequestID).Scan(&events))
	require.Equal(t, 1, events)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs WHERE action='admin.video.refund' AND extra->>'target_request_id'=$1`, task.RequestID).Scan(&audits))
	require.Equal(t, 1, audits)
}

func TestAdminVideoRefundSameKeyRaceReplaysAndDifferentPayloadConflicts(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	taskRepo := NewVideoTaskRepository(integrationDB)
	billing := NewUsageBillingRepository(client, integrationDB)
	task, _, user := createVideoBillingTask(t, client, taskRepo, "admin-refund-same-key", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))
	_, err := integrationDB.ExecContext(ctx, `UPDATE video_tasks SET status='unknown', billing_status='held' WHERE request_id=$1`, task.RequestID)
	require.NoError(t, err)

	repo := NewAdminVideoRepository(integrationDB)
	command := service.AdminVideoRefundMutation{
		RequestID: task.RequestID,
		AdminVideoActionMetadata: service.AdminVideoActionMetadata{
			ActorUserID: 7, Reason: "provider confirmed absent", IdempotencyKeyHash: service.HashIdempotencyKey("same-key"),
			RequestHash: service.HashIdempotencyKey("same-payload"),
		},
	}
	var wg sync.WaitGroup
	results := make(chan *service.AdminVideoActionResult, 2)
	errorsCh := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, actionErr := repo.Refund(ctx, command)
			results <- result
			errorsCh <- actionErr
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for actionErr := range errorsCh {
		require.NoError(t, actionErr)
	}
	var replayed int
	for result := range results {
		require.NotNil(t, result)
		if result.Replayed {
			replayed++
		}
	}
	require.Equal(t, 1, replayed)
	assertVideoBalanceExact(t, user.ID, "10", "0")

	conflict := command
	conflict.RequestHash = service.HashIdempotencyKey("different-payload")
	_, err = repo.Refund(ctx, conflict)
	require.ErrorIs(t, err, service.ErrIdempotencyKeyConflict)
}

func TestAdminVideoCompleteRollsBackFinanceTaskEventAndAuditTogether(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	taskRepo := NewVideoTaskRepository(integrationDB)
	billing := NewUsageBillingRepository(client, integrationDB)
	task, apiKey, user := createVideoBillingTask(t, client, taskRepo, "admin-complete-rollback", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))
	_, err := integrationDB.ExecContext(ctx, `UPDATE video_tasks SET status='unknown', billing_status='held' WHERE request_id=$1`, task.RequestID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION admin_video_test_reject_event() RETURNS TRIGGER AS $$
		BEGIN IF NEW.request_id = '`+task.RequestID+`' AND NEW.event_type = 'admin_complete' THEN RAISE EXCEPTION 'injected event failure'; END IF; RETURN NEW; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_admin_video_test_reject_event BEFORE INSERT ON video_task_events FOR EACH ROW EXECUTE FUNCTION admin_video_test_reject_event();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_admin_video_test_reject_event ON video_task_events`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS admin_video_test_reject_event()`)
	})

	repo := NewAdminVideoRepository(integrationDB)
	_, err = repo.Complete(ctx, service.AdminVideoCompleteMutation{
		RequestID: task.RequestID, AdminVideoActionMetadata: service.AdminVideoActionMetadata{ActorUserID: 7, Reason: "verified", IdempotencyKeyHash: service.HashIdempotencyKey("complete"),
			RequestHash: service.HashIdempotencyKey("complete-payload")}, ProviderTaskID: "provider-task", ResultURL: "https://cdn.example.com/video.mp4?token=secret",
		ResultURLAuditSummary: "https://cdn.example.com/video.mp4", DurationSeconds: 6, Resolution: "720p", FinalAmount: 1.5,
		StoredUnitPrice: task.UnitPrice,
	})
	require.ErrorContains(t, err, "injected event failure")
	assertVideoBalanceExact(t, user.ID, "8", "2")
	var captureClaims, actions, audits int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, service.VideoCaptureRequestID(task.RequestID), apiKey.ID).Scan(&captureClaims))
	require.Zero(t, captureClaims)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_admin_actions WHERE request_id=$1`, task.RequestID).Scan(&actions))
	require.Zero(t, actions)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs WHERE action='admin.video.complete' AND extra->>'target_request_id'=$1`, task.RequestID).Scan(&audits))
	require.Zero(t, audits)
}

func TestAdminVideoMigration203CanApplyAndRollbackInOneTransaction(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS video_ops_metrics; DROP TABLE IF EXISTS video_admin_actions; ALTER TABLE video_tasks DROP COLUMN IF EXISTS upstream_unit_cost; ALTER TABLE video_pricing_rules DROP CONSTRAINT IF EXISTS video_pricing_rules_finite_nonnegative_prices`)
	require.NoError(t, err)
	migration, err := migrationfiles.FS.ReadFile("203_admin_video_operations.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	for _, table := range []string{"video_admin_actions", "video_ops_metrics"} {
		var regclass string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT $1::regclass::text`, table).Scan(&regclass))
		require.Equal(t, table, regclass)
	}
	require.NoError(t, tx.Rollback())
	var installed bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT to_regclass('video_admin_actions') IS NOT NULL`).Scan(&installed))
	require.True(t, installed, fmt.Sprintf("installed migration must remain after rollback probe"))
}
