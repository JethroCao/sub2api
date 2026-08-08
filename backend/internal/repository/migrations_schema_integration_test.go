//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	migrationfiles "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const originalVideoBillingPrecisionMigration198 = `-- Align the balance ledger with the ten-decimal video/subscription ledger.
-- Precision 22 preserves the legacy twelve-digit integer range of DECIMAL(20,8).

ALTER TABLE users
    ALTER COLUMN balance TYPE DECIMAL(22,10) USING balance::DECIMAL(22,10),
    ALTER COLUMN frozen_balance TYPE DECIMAL(22,10) USING frozen_balance::DECIMAL(22,10);
`

func TestMigrationsUpgradeFromOriginal196AppliesForward200AfterExplicitDuplicateRepair(t *testing.T) {
	const preflightMessage = "migration 200 cannot enforce video idempotency scope: duplicate non-empty idempotency keys exist for the same user_id and api_key_id across video operations; reconcile each task and its billing hold explicitly, ensure only one task retains each key, then retry migration 200"
	ctx := context.Background()
	repo := NewVideoTaskRepository(integrationDB)
	firstParams := videoTaskCreateParams(t, "migration-200-generation", videoTaskHash("migration-200-generation-"+t.Name()))
	first, created, err := repo.CreateOrGet(ctx, firstParams)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, first.RequestID)

	secondParams := firstParams
	secondParams.Operation = "edit"
	secondParams.IdempotencyKeyHash = videoTaskHash("migration-200-edit-" + t.Name())
	secondParams.RequestHash = videoTaskHash("migration-200-edit-request-" + t.Name())
	second, created, err := repo.CreateOrGet(ctx, secondParams)
	require.NoError(t, err)
	require.True(t, created)
	cleanupVideoTask(t, second.RequestID)

	migration, err := migrationfiles.FS.ReadFile("200_video_idempotency_scope.sql")
	require.NoError(t, err)
	migrationFS := fstest.MapFS{"200_video_idempotency_scope.sql": {Data: migration}}

	restore := func() {
		_, _ = integrationDB.ExecContext(context.Background(), `UPDATE video_tasks SET idempotency_key_hash = $2 WHERE request_id = $1`, second.RequestID, secondParams.IdempotencyKeyHash)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP INDEX IF EXISTS idx_video_tasks_idempotency`)
		_, _ = integrationDB.ExecContext(context.Background(), `CREATE UNIQUE INDEX idx_video_tasks_idempotency ON video_tasks (user_id, api_key_id, operation, idempotency_key_hash) WHERE idempotency_key_hash <> ''`)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE filename = '200_video_idempotency_scope.sql'`)
		_ = applyMigrationsFS(context.Background(), integrationDB, migrationFS)
	}
	t.Cleanup(restore)

	_, err = integrationDB.ExecContext(ctx, `DROP INDEX idx_video_tasks_idempotency`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `CREATE UNIQUE INDEX idx_video_tasks_idempotency ON video_tasks (user_id, api_key_id, operation, idempotency_key_hash) WHERE idempotency_key_hash <> ''`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename = '200_video_idempotency_scope.sql'`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET idempotency_key_hash = $2 WHERE request_id = $1`, second.RequestID, firstParams.IdempotencyKeyHash)
	require.NoError(t, err)

	migrationErr := applyMigrationsFS(ctx, integrationDB, migrationFS)
	require.Error(t, migrationErr)
	var pqErr *pq.Error
	require.ErrorAs(t, migrationErr, &pqErr)
	require.Equal(t, pq.ErrorCode("23505"), pqErr.Code)
	require.Equal(t, preflightMessage, pqErr.Message)
	require.Empty(t, pqErr.Detail)
	require.Empty(t, pqErr.Hint)

	var applied, duplicates int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE filename = '200_video_idempotency_scope.sql'`).Scan(&applied))
	require.Zero(t, applied)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tasks WHERE user_id = $1 AND api_key_id = $2 AND idempotency_key_hash = $3`, first.UserID, first.APIKeyID, firstParams.IdempotencyKeyHash).Scan(&duplicates))
	require.Equal(t, 2, duplicates, "failed preflight must preserve every historical task")
	var definition string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_video_tasks_idempotency'`).Scan(&definition))
	require.Contains(t, definition, "operation")

	_, err = integrationDB.ExecContext(ctx, `UPDATE video_tasks SET idempotency_key_hash = $2 WHERE request_id = $1`, second.RequestID, secondParams.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NoError(t, applyMigrationsFS(ctx, integrationDB, migrationFS))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_video_tasks_idempotency'`).Scan(&definition))
	require.NotContains(t, definition, "operation")
	require.Contains(t, definition, "user_id, api_key_id, idempotency_key_hash")
}

func TestMigrationsRunner_ConcurrentInstancesSerializeOnSessionLock(t *testing.T) {
	const instances = 2
	errorsByInstance := make([]error, instances)
	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errorsByInstance[index] = ApplyMigrations(ctx, integrationDB)
		}(i)
	}
	wg.Wait()
	for i, err := range errorsByInstance {
		require.NoErrorf(t, err, "migration instance %d", i)
	}
}

func TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate(t *testing.T) {
	tx := testTx(t)

	// Re-apply migrations to verify idempotency (no errors, no duplicate rows).
	require.NoError(t, ApplyMigrations(context.Background(), integrationDB))

	// schema_migrations should have at least the current migration set.
	var applied int
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&applied))
	require.GreaterOrEqual(t, applied, 7, "expected schema_migrations to contain applied migrations")

	// users: columns required by repository queries
	requireColumn(t, tx, "users", "username", "character varying", 100, false)
	requireColumn(t, tx, "users", "notes", "text", 0, false)
	requireNumericColumn(t, tx, "users", "balance", 20, 8)
	requireNumericColumn(t, tx, "users", "frozen_balance", 20, 8)
	requireNumericColumn(t, tx, "video_tasks", "estimated_amount", 20, 8)
	requireNumericColumn(t, tx, "video_tasks", "frozen_amount", 20, 8)
	requireNumericColumn(t, tx, "video_tasks", "settled_amount", 20, 8)

	// accounts: schedulable and rate-limit fields
	requireColumn(t, tx, "accounts", "notes", "text", 0, true)
	requireColumn(t, tx, "accounts", "schedulable", "boolean", 0, false)
	requireColumn(t, tx, "accounts", "rate_limited_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "rate_limit_reset_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "overload_until", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "session_window_status", "character varying", 20, true)
	requireIndex(t, tx, "accounts", "idx_accounts_autopause_expiry_due")

	// groups: OpenAI Live 默认关闭，管理员显式开启后才可访问。
	requireColumn(t, tx, "groups", "allow_live", "boolean", 0, false)

	// api_keys: key length should be 128
	requireColumn(t, tx, "api_keys", "key", "character varying", 128, false)

	// redeem_codes: subscription fields
	requireColumn(t, tx, "redeem_codes", "group_id", "bigint", 0, true)
	requireColumn(t, tx, "redeem_codes", "validity_days", "integer", 0, false)

	// usage_logs: billing_type used by filters/stats
	requireColumn(t, tx, "usage_logs", "billing_type", "smallint", 0, false)
	requireColumn(t, tx, "usage_logs", "request_type", "smallint", 0, false)
	requireColumn(t, tx, "usage_logs", "openai_ws_mode", "boolean", 0, false)
	requireColumn(t, tx, "usage_logs", "image_input_size", "character varying", 32, true)
	requireColumn(t, tx, "usage_logs", "image_output_size", "character varying", 32, true)
	requireColumn(t, tx, "usage_logs", "image_size_source", "character varying", 16, true)
	requireColumn(t, tx, "usage_logs", "image_size_breakdown", "jsonb", 0, true)
	requireColumn(t, tx, "usage_logs", "video_count", "integer", 0, false)
	requireColumn(t, tx, "usage_logs", "video_resolution", "character varying", 10, true)
	requireColumn(t, tx, "usage_logs", "video_duration_seconds", "integer", 0, true)
	requireConstraintDefinitionContains(
		t,
		tx,
		"usage_logs",
		"usage_logs_image_size_source_check",
		"image_size_source",
		"'output'",
		"'input'",
		"'default'",
		"'legacy'",
	)
	requireConstraintDefinitionContains(
		t,
		tx,
		"usage_logs",
		"usage_logs_image_billing_size_check",
		"image_count",
		"billing_mode",
		"'video'",
		"video_count",
		"image_size IS NOT NULL",
		"'1K'",
		"'2K'",
		"'4K'",
		"'mixed'",
	)

	// usage_billing_dedup: billing idempotency narrow table
	var usageBillingDedupRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.usage_billing_dedup')").Scan(&usageBillingDedupRegclass))
	require.True(t, usageBillingDedupRegclass.Valid, "expected usage_billing_dedup table to exist")
	requireColumn(t, tx, "usage_billing_dedup", "request_fingerprint", "character varying", 64, false)
	requireIndex(t, tx, "usage_billing_dedup", "idx_usage_billing_dedup_request_api_key")
	requireIndex(t, tx, "usage_billing_dedup", "idx_usage_billing_dedup_created_at_brin")

	var usageBillingDedupArchiveRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.usage_billing_dedup_archive')").Scan(&usageBillingDedupArchiveRegclass))
	require.True(t, usageBillingDedupArchiveRegclass.Valid, "expected usage_billing_dedup_archive table to exist")
	requireColumn(t, tx, "usage_billing_dedup_archive", "request_fingerprint", "character varying", 64, false)
	requireIndex(t, tx, "usage_billing_dedup_archive", "usage_billing_dedup_archive_pkey")

	// settings table should exist
	var settingsRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.settings')").Scan(&settingsRegclass))
	require.True(t, settingsRegclass.Valid, "expected settings table to exist")

	// security_secrets table should exist
	var securitySecretsRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.security_secrets')").Scan(&securitySecretsRegclass))
	require.True(t, securitySecretsRegclass.Valid, "expected security_secrets table to exist")

	// scheduler_outbox pending dedup support
	requireColumn(t, tx, "scheduler_outbox", "dedup_key", "text", 0, true)
	requireIndex(t, tx, "scheduler_outbox", "idx_scheduler_outbox_pending_dedup_key")

	// ops_system_logs: API key id index for operational log triage
	requireColumn(t, tx, "ops_system_logs", "api_key_id", "bigint", 0, true)
	requireIndex(t, tx, "ops_system_logs", "idx_ops_system_logs_api_key_id_created_at")

	// Bounded ingress rejection security aggregates.
	requireColumn(t, tx, "ops_ingress_reject_aggregates", "bucket_start", "timestamp with time zone", 0, false)
	requireColumn(t, tx, "ops_ingress_reject_aggregates", "client_ip", "inet", 0, false)
	requireColumn(t, tx, "ops_ingress_reject_aggregates", "request_count", "bigint", 0, false)
	requireIndex(t, tx, "ops_ingress_reject_aggregates", "idx_ops_ingress_reject_aggregates_bucket")
	requireIndex(t, tx, "ops_ingress_reject_aggregates", "idx_ops_ingress_reject_aggregates_ip_bucket")

	// user_allowed_groups table should exist
	var uagRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.user_allowed_groups')").Scan(&uagRegclass))
	require.True(t, uagRegclass.Valid, "expected user_allowed_groups table to exist")

	// user_subscriptions: deleted_at for soft delete support (migration 012)
	requireColumn(t, tx, "user_subscriptions", "deleted_at", "timestamp with time zone", 0, true)

	// orphan_allowed_groups_audit table should exist (migration 013)
	var orphanAuditRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.orphan_allowed_groups_audit')").Scan(&orphanAuditRegclass))
	require.True(t, orphanAuditRegclass.Valid, "expected orphan_allowed_groups_audit table to exist")

	// account_groups: created_at should be timestamptz
	requireColumn(t, tx, "account_groups", "created_at", "timestamp with time zone", 0, false)

	// user_allowed_groups: created_at should be timestamptz
	requireColumn(t, tx, "user_allowed_groups", "created_at", "timestamp with time zone", 0, false)
}

func TestDurableGrokVideoRouteMigrationEnforcesExactPlatformProviderPairs(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	rolledBack := false
	t.Cleanup(func() {
		if !rolledBack {
			_ = tx.Rollback()
		}
	})
	_, err = tx.ExecContext(ctx, `
		ALTER TABLE video_tasks
			DROP CONSTRAINT video_tasks_platform_provider_check,
			DROP CONSTRAINT video_tasks_platform_check,
			DROP CONSTRAINT video_tasks_provider_check;
		ALTER TABLE video_tasks
			ADD CONSTRAINT video_tasks_platform_check CHECK (platform = 'video'),
			ADD CONSTRAINT video_tasks_provider_check CHECK (provider IN ('seedance', 'kling'));
	`)
	require.NoError(t, err)
	migration, err := migrationfiles.FS.ReadFile("202_durable_grok_video_route.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)

	valid := []struct{ platform, provider string }{
		{service.PlatformGrok, service.PlatformGrok},
		{service.PlatformVideo, service.VideoProviderSeedance},
		{service.PlatformVideo, service.VideoProviderKling},
	}
	invalid := []struct{ platform, provider string }{
		{service.PlatformGrok, service.VideoProviderSeedance},
		{service.PlatformVideo, service.PlatformGrok},
		{"other", service.PlatformGrok},
		{service.PlatformVideo, "other"},
	}

	insert := func(requestID, platform, provider string) error {
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO video_tasks (
				request_id, user_id, api_key_id, group_id, account_id, platform, provider,
				operation, external_model, upstream_model, request_hash, pricing_unit,
				unit_price, estimated_units, estimated_amount, frozen_amount, currency,
				billing_mode, billing_status
			) VALUES ($1, 1, 1, 1, 0, $2, $3, 'generation', 'model', '', $4,
				'per_request', 0, 1, 0, 0, 'USD', 'balance', 'held')`,
			requestID, platform, provider, strings.Repeat("a", 64))
		return insertErr
	}
	for i, route := range valid {
		requestID := "vid_" + fmt.Sprintf("%032x", 9000+i)
		require.NoError(t, insert(requestID, route.platform, route.provider), "route=%s/%s", route.platform, route.provider)
	}
	for i, route := range invalid {
		_, err = tx.ExecContext(ctx, "SAVEPOINT invalid_video_route")
		require.NoError(t, err)
		requestID := "vid_" + fmt.Sprintf("%032x", 9100+i)
		require.Error(t, insert(requestID, route.platform, route.provider), "route=%s/%s", route.platform, route.provider)
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT invalid_video_route")
		require.NoError(t, err)
	}
	var kept int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tasks WHERE request_id >= 'vid_00000000000000000000000000002328' AND request_id <= 'vid_0000000000000000000000000000232a'`).Scan(&kept))
	require.Equal(t, len(valid), kept, "failed constraint probes must preserve valid rows in the migration transaction")

	require.NoError(t, tx.Rollback())
	rolledBack = true
	var definition string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'video_tasks'::regclass AND conname = 'video_tasks_platform_provider_check'`).Scan(&definition))
	require.Contains(t, definition, "(platform)::text = 'grok'::text")
	require.Contains(t, definition, "(provider)::text = 'grok'::text")
}

func TestMigrationsUpgradeFromOriginal198AppliesForward199(t *testing.T) {
	const (
		balancePreflightError = "migration 199 preflight failed: users.balance contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate users.balance so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199"
		frozenPreflightError  = "migration 199 preflight failed: users.frozen_balance contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate users.frozen_balance so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199"
	)

	ctx := context.Background()
	client := testEntClient(t)
	taskRepo := NewVideoTaskRepository(integrationDB)
	task, _, upperUser := createVideoBillingTask(t, client, taskRepo, "migration-198-upgrade-upper", "balance", nil, 1, 1, nil, 0)
	videoBoundaryTask, _, lowerUser := createVideoBillingTask(t, client, taskRepo, "migration-198-upgrade-lower", "balance", nil, 1, 1, nil, 0)

	_, err := integrationDB.ExecContext(ctx, `
		ALTER TABLE video_tasks
			ALTER COLUMN estimated_amount TYPE DECIMAL(20,10) USING estimated_amount::DECIMAL(20,10),
			ALTER COLUMN frozen_amount TYPE DECIMAL(20,10) USING frozen_amount::DECIMAL(20,10),
			ALTER COLUMN settled_amount TYPE DECIMAL(20,10) USING settled_amount::DECIMAL(20,10)
	`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename IN ('198_unified_video_billing_precision.sql', '199_restore_video_billing_ledger_scale.sql')`)
	require.NoError(t, err)

	originalFS := fstest.MapFS{
		"198_unified_video_billing_precision.sql": {Data: []byte(originalVideoBillingPrecisionMigration198)},
	}
	require.Equal(t, "72cce8db8f5371c4e52c8650ac1e06379446d1e70f65782be2d6a18b73383413", migrationChecksum(originalVideoBillingPrecisionMigration198))
	require.NoError(t, applyMigrationsFS(ctx, integrationDB, originalFS))

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE users
		SET balance = 999999999999.9999999949,
			frozen_balance = 0.0000000050
		WHERE id = $1
	`, upperUser.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE users
		SET balance = -999999999999.9999999949,
			frozen_balance = 0.0000000050
		WHERE id = $1
	`, lowerUser.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE video_tasks
		SET status = 'succeeded', estimated_amount = 0.1234567850,
			frozen_amount = 1.0000000050, settled_amount = 0.3000000050,
			settled_at = NOW()
		WHERE request_id = $1
	`, task.RequestID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE video_tasks
		SET status = 'succeeded', estimated_amount = 9999999999.9999999999,
			frozen_amount = 9999999999.9999999999,
			settled_amount = 9999999999.9999999999,
			settled_at = NOW()
		WHERE request_id = $1
	`, videoBoundaryTask.RequestID)
	require.NoError(t, err)

	blockerTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	blockerDone := false
	t.Cleanup(func() {
		if !blockerDone {
			_ = blockerTx.Rollback()
		}
	})
	_, err = blockerTx.ExecContext(ctx, `UPDATE users SET balance = balance WHERE id = $1`, upperUser.ID)
	require.NoError(t, err)

	upperBalanceMigrationResult := make(chan error, 1)
	go func() {
		upperBalanceMigrationResult <- ApplyMigrations(ctx, integrationDB)
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := integrationDB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE relation = 'users'::regclass
				  AND mode = 'AccessExclusiveLock'
				  AND NOT granted
			)
		`).Scan(&waiting)
		return queryErr == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "migration 199 did not reach the users table lock")

	_, err = blockerTx.ExecContext(ctx, `UPDATE users SET balance = 999999999999.9999999950 WHERE id = $1`, upperUser.ID)
	require.NoError(t, err)
	require.NoError(t, blockerTx.Commit())
	blockerDone = true

	var upperBalanceMigrationErr error
	select {
	case upperBalanceMigrationErr = <-upperBalanceMigrationResult:
	case <-time.After(10 * time.Second):
		t.Fatal("migration 199 did not return after the concurrent users.balance write committed")
	}
	assertMigration199PreflightError(t, upperBalanceMigrationErr, balancePreflightError)
	requireMigration199UnappliedAtOriginal198State(t)

	var valuesMatch bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT upper_user.balance = 999999999999.9999999950
		   AND upper_user.frozen_balance = 0.0000000050
		   AND lower_user.balance = -999999999999.9999999949
		   AND lower_user.frozen_balance = 0.0000000050
		   AND vt.estimated_amount = 0.1234567850
		   AND vt.frozen_amount = 1.0000000050
		   AND vt.settled_amount = 0.3000000050
		   AND boundary_vt.estimated_amount = 9999999999.9999999999
		   AND boundary_vt.frozen_amount = 9999999999.9999999999
		   AND boundary_vt.settled_amount = 9999999999.9999999999
		FROM users AS upper_user
		JOIN users AS lower_user ON lower_user.id = $2
		JOIN video_tasks AS vt ON vt.request_id = $3
		JOIN video_tasks AS boundary_vt ON boundary_vt.request_id = $4
		WHERE upper_user.id = $1
	`, upperUser.ID, lowerUser.ID, task.RequestID, videoBoundaryTask.RequestID).Scan(&valuesMatch))
	require.True(t, valuesMatch, "failed migration must preserve every source value")

	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET balance = 999999999999.9999999949 WHERE id = $1`, upperUser.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET balance = -999999999999.9999999950 WHERE id = $1`, lowerUser.ID)
	require.NoError(t, err)

	lowerBalanceMigrationErr := ApplyMigrations(ctx, integrationDB)
	assertMigration199PreflightError(t, lowerBalanceMigrationErr, balancePreflightError)
	requireMigration199UnappliedAtOriginal198State(t)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT upper_user.balance = 999999999999.9999999949
		   AND upper_user.frozen_balance = 0.0000000050
		   AND lower_user.balance = -999999999999.9999999950
		FROM users AS upper_user
		JOIN users AS lower_user ON lower_user.id = $2
		WHERE upper_user.id = $1
	`, upperUser.ID, lowerUser.ID).Scan(&valuesMatch))
	require.True(t, valuesMatch, "lower balance preflight must not mutate the repaired upper balance")

	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET balance = -999999999999.9999999949 WHERE id = $1`, lowerUser.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET frozen_balance = 999999999999.9999999950 WHERE id = $1`, upperUser.ID)
	require.NoError(t, err)

	frozenBalanceMigrationErr := ApplyMigrations(ctx, integrationDB)
	assertMigration199PreflightError(t, frozenBalanceMigrationErr, frozenPreflightError)
	requireMigration199UnappliedAtOriginal198State(t)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT upper_user.balance = 999999999999.9999999949
		   AND upper_user.frozen_balance = 999999999999.9999999950
		   AND lower_user.balance = -999999999999.9999999949
		FROM users AS upper_user
		JOIN users AS lower_user ON lower_user.id = $2
		WHERE upper_user.id = $1
	`, upperUser.ID, lowerUser.ID).Scan(&valuesMatch))
	require.True(t, valuesMatch, "frozen-balance preflight must not mutate corrected balances")

	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET frozen_balance = 999999999999.9999999949 WHERE id = $1`, upperUser.ID)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, integrationDB))
	requireNumericColumnOnDB(t, "users", "balance", 20, 8)
	requireNumericColumnOnDB(t, "users", "frozen_balance", 20, 8)
	requireNumericColumnOnDB(t, "video_tasks", "estimated_amount", 20, 8)
	requireNumericColumnOnDB(t, "video_tasks", "frozen_amount", 20, 8)
	requireNumericColumnOnDB(t, "video_tasks", "settled_amount", 20, 8)

	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT upper_user.balance = 999999999999.99999999
		   AND upper_user.frozen_balance = 999999999999.99999999
		   AND lower_user.balance = -999999999999.99999999
		   AND lower_user.frozen_balance = 0.00000001
		   AND vt.estimated_amount = 0.12345679
		   AND vt.frozen_amount = 1.00000001
		   AND vt.settled_amount = 0.30000001
		   AND boundary_vt.estimated_amount = 10000000000.00000000
		   AND boundary_vt.frozen_amount = 10000000000.00000000
		   AND boundary_vt.settled_amount = 10000000000.00000000
		FROM users AS upper_user
		JOIN users AS lower_user ON lower_user.id = $2
		JOIN video_tasks AS vt ON vt.request_id = $3
		JOIN video_tasks AS boundary_vt ON boundary_vt.request_id = $4
		WHERE upper_user.id = $1
	`, upperUser.ID, lowerUser.ID, task.RequestID, videoBoundaryTask.RequestID).Scan(&valuesMatch))
	require.True(t, valuesMatch)

	var originalChecksum, forwardChecksum string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE filename = '198_unified_video_billing_precision.sql'`).Scan(&originalChecksum))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE filename = '199_restore_video_billing_ledger_scale.sql'`).Scan(&forwardChecksum))
	require.Equal(t, "72cce8db8f5371c4e52c8650ac1e06379446d1e70f65782be2d6a18b73383413", originalChecksum)
	require.NotEmpty(t, forwardChecksum)
}

func assertMigration199PreflightError(t *testing.T, err error, message string) {
	t.Helper()

	if !assert.Error(t, err) {
		return
	}
	var pqErr *pq.Error
	if !assert.True(t, errors.As(err, &pqErr), "expected wrapped PostgreSQL error, got %T: %v", err, err) {
		return
	}
	assert.Equal(t, pq.ErrorCode("22003"), pqErr.Code)
	assert.Equal(t, message, pqErr.Message)
	assert.Empty(t, pqErr.Detail, "preflight must not expose source row data")
	assert.Empty(t, pqErr.Hint, "preflight remediation belongs in the stable message")
}

func requireMigration199UnappliedAtOriginal198State(t *testing.T) {
	t.Helper()

	var applied int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*)
		FROM schema_migrations
		WHERE filename = '199_restore_video_billing_ledger_scale.sql'
	`).Scan(&applied))
	require.Zero(t, applied, "migration 199 must remain unapplied after a failed preflight")

	var originalChecksum string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT checksum
		FROM schema_migrations
		WHERE filename = '198_unified_video_billing_precision.sql'
	`).Scan(&originalChecksum))
	require.Equal(t, "72cce8db8f5371c4e52c8650ac1e06379446d1e70f65782be2d6a18b73383413", originalChecksum)

	requireNumericColumnOnDB(t, "users", "balance", 22, 10)
	requireNumericColumnOnDB(t, "users", "frozen_balance", 22, 10)
	requireNumericColumnOnDB(t, "video_tasks", "estimated_amount", 20, 10)
	requireNumericColumnOnDB(t, "video_tasks", "frozen_amount", 20, 10)
	requireNumericColumnOnDB(t, "video_tasks", "settled_amount", 20, 10)
}

func requireNumericColumnOnDB(t *testing.T, table, column string, precision, scale int) {
	t.Helper()

	var actualType string
	var actualPrecision, actualScale sql.NullInt64
	err := integrationDB.QueryRowContext(context.Background(), `
		SELECT data_type, numeric_precision, numeric_scale
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
	`, table, column).Scan(&actualType, &actualPrecision, &actualScale)
	require.NoError(t, err)
	require.Equal(t, "numeric", actualType)
	require.Equal(t, int64(precision), actualPrecision.Int64)
	require.Equal(t, int64(scale), actualScale.Int64)
}

func TestMigrationsRunner_AuthIdentityAndPaymentSchemaStayAligned(t *testing.T) {
	tx := testTx(t)

	requireColumn(t, tx, "auth_identity_migration_reports", "report_type", "character varying", 80, false)
	requireColumn(t, tx, "users", "signup_source", "character varying", 20, false)
	requireColumnDefaultContains(t, tx, "users", "signup_source", "email")
	requireConstraintDefinitionContains(
		t,
		tx,
		"users",
		"users_signup_source_check",
		"signup_source",
		"'email'",
		"'linuxdo'",
		"'wechat'",
		"'oidc'",
	)

	requireForeignKeyOnDelete(t, tx, "auth_identities", "user_id", "users", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "auth_identity_channels", "identity_id", "auth_identities", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "pending_auth_sessions", "target_user_id", "users", "SET NULL")
	requireForeignKeyOnDelete(t, tx, "identity_adoption_decisions", "pending_auth_session_id", "pending_auth_sessions", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "identity_adoption_decisions", "identity_id", "auth_identities", "SET NULL")

	requireIndex(t, tx, "payment_orders", "paymentorder_out_trade_no")
	requirePartialUniqueIndexDefinition(t, tx, "payment_orders", "paymentorder_out_trade_no", "out_trade_no", "WHERE")
	requireIndexAbsent(t, tx, "payment_orders", "paymentorder_out_trade_no_unique")
}

func requireIndex(t *testing.T, tx *sql.Tx, table, index string) {
	t.Helper()

	var exists bool
	err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1
	FROM pg_indexes
	WHERE schemaname = 'public'
	  AND tablename = $1
	  AND indexname = $2
)
`, table, index).Scan(&exists)
	require.NoError(t, err, "query pg_indexes for %s.%s", table, index)
	require.True(t, exists, "expected index %s on %s", index, table)
}

func requireIndexAbsent(t *testing.T, tx *sql.Tx, table, index string) {
	t.Helper()

	var exists bool
	err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1
	FROM pg_indexes
	WHERE schemaname = 'public'
	  AND tablename = $1
	  AND indexname = $2
)
`, table, index).Scan(&exists)
	require.NoError(t, err, "query pg_indexes for %s.%s", table, index)
	require.False(t, exists, "expected index %s on %s to be absent", index, table)
}

func requirePartialUniqueIndexDefinition(t *testing.T, tx *sql.Tx, table, index string, fragments ...string) {
	t.Helper()

	var (
		unique bool
		def    string
	)

	err := tx.QueryRowContext(context.Background(), `
SELECT
	i.indisunique,
	pg_get_indexdef(i.indexrelid)
FROM pg_class idx
JOIN pg_index i ON i.indexrelid = idx.oid
JOIN pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND idx.relname = $2
`, table, index).Scan(&unique, &def)
	require.NoError(t, err, "query index definition for %s.%s", table, index)
	require.True(t, unique, "expected index %s on %s to be unique", index, table)

	for _, fragment := range fragments {
		require.Contains(t, def, fragment, "expected index definition for %s.%s to contain %q", table, index, fragment)
	}
}

func requireForeignKeyOnDelete(t *testing.T, tx *sql.Tx, table, column, refTable, expected string) {
	t.Helper()

	var actual string
	err := tx.QueryRowContext(context.Background(), `
SELECT CASE c.confdeltype
	WHEN 'a' THEN 'NO ACTION'
	WHEN 'r' THEN 'RESTRICT'
	WHEN 'c' THEN 'CASCADE'
	WHEN 'n' THEN 'SET NULL'
	WHEN 'd' THEN 'SET DEFAULT'
END
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
JOIN pg_class ref_tbl ON ref_tbl.oid = c.confrelid
JOIN pg_attribute attr ON attr.attrelid = tbl.oid AND attr.attnum = ANY(c.conkey)
WHERE ns.nspname = 'public'
  AND c.contype = 'f'
  AND tbl.relname = $1
  AND attr.attname = $2
  AND ref_tbl.relname = $3
LIMIT 1
`, table, column, refTable).Scan(&actual)
	require.NoError(t, err, "query foreign key action for %s.%s -> %s", table, column, refTable)
	require.Equal(t, expected, actual, "unexpected ON DELETE action for %s.%s -> %s", table, column, refTable)
}

func requireConstraintDefinitionContains(t *testing.T, tx *sql.Tx, table, constraint string, fragments ...string) {
	t.Helper()

	var def string
	err := tx.QueryRowContext(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND c.conname = $2
`, table, constraint).Scan(&def)
	require.NoError(t, err, "query constraint definition for %s.%s", table, constraint)

	for _, fragment := range fragments {
		require.Contains(t, def, fragment, "expected constraint definition for %s.%s to contain %q", table, constraint, fragment)
	}
}

func requireColumnDefaultContains(t *testing.T, tx *sql.Tx, table, column string, fragments ...string) {
	t.Helper()

	var columnDefault sql.NullString
	err := tx.QueryRowContext(context.Background(), `
SELECT column_default
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&columnDefault)
	require.NoError(t, err, "query column_default for %s.%s", table, column)
	require.True(t, columnDefault.Valid, "expected column_default for %s.%s", table, column)

	for _, fragment := range fragments {
		require.Contains(t, columnDefault.String, fragment, "expected default for %s.%s to contain %q", table, column, fragment)
	}
}

func requireColumn(t *testing.T, tx *sql.Tx, table, column, dataType string, maxLen int, nullable bool) {
	t.Helper()

	var row struct {
		DataType string
		MaxLen   sql.NullInt64
		Nullable string
	}

	err := tx.QueryRowContext(context.Background(), `
SELECT
  data_type,
  character_maximum_length,
  is_nullable
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&row.DataType, &row.MaxLen, &row.Nullable)
	require.NoError(t, err, "query information_schema.columns for %s.%s", table, column)
	require.Equal(t, dataType, row.DataType, "data_type mismatch for %s.%s", table, column)

	if maxLen > 0 {
		require.True(t, row.MaxLen.Valid, "expected maxLen for %s.%s", table, column)
		require.Equal(t, int64(maxLen), row.MaxLen.Int64, "maxLen mismatch for %s.%s", table, column)
	}

	if nullable {
		require.Equal(t, "YES", row.Nullable, "nullable mismatch for %s.%s", table, column)
	} else {
		require.Equal(t, "NO", row.Nullable, "nullable mismatch for %s.%s", table, column)
	}
}

func requireNumericColumn(t *testing.T, tx *sql.Tx, table, column string, precision, scale int) {
	t.Helper()
	var actualType string
	var actualPrecision, actualScale sql.NullInt64
	err := tx.QueryRowContext(context.Background(), `
		SELECT data_type, numeric_precision, numeric_scale
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
	`, table, column).Scan(&actualType, &actualPrecision, &actualScale)
	require.NoError(t, err)
	require.Equal(t, "numeric", actualType)
	require.Equal(t, int64(precision), actualPrecision.Int64)
	require.Equal(t, int64(scale), actualScale.Int64)
}
