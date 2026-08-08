package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnifiedVideoMigrationsDefinePermissionPricingAndInvalidation(t *testing.T) {
	foundation, err := FS.ReadFile("194_unified_video_group_pricing.sql")
	require.NoError(t, err)
	sql := string(foundation)
	require.Contains(t, sql, "allow_video_generation BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS video_pricing_rules")
	require.Contains(t, sql, "UNIQUE (group_id, external_model, operation, resolution, audio_mode)")

	cache, err := FS.ReadFile("195_unified_video_auth_cache_invalidation.sql")
	require.NoError(t, err)
	require.Contains(t, string(cache), "OLD.allow_video_generation IS NOT DISTINCT FROM NEW.allow_video_generation")
}

func TestUnifiedVideoMigrationsDefineTasks(t *testing.T) {
	tasks, err := FS.ReadFile("196_unified_video_tasks.sql")
	require.NoError(t, err)
	sql := string(tasks)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS video_tasks")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS video_task_events")
	require.Contains(t, sql, "WHERE idempotency_key_hash <> ''")
	require.Contains(t, sql, "CHECK (request_id ~ '^vid_[0-9a-f]{32}$')")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_video_tasks_status_next_poll")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_video_tasks_lease_expires")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_video_tasks_upstream_identity")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_video_tasks_owner")
	require.Contains(t, sql, "video task events are append-only")
	require.Contains(t, sql, "CONSTRAINT video_tasks_upstream_payload_cleared")
	require.Contains(t, sql, "upstream_task_id IS NULL OR request_payload IS NULL")
	require.Contains(t, sql, "CONSTRAINT video_tasks_finite_amounts")
	require.Contains(t, sql, "unit_price < 'Infinity'::numeric")
	require.Contains(t, sql, "CONSTRAINT video_tasks_settlement_status")
	require.Contains(t, sql, "CONSTRAINT video_tasks_settlement_complete")
}

func TestUnifiedVideoSubscriptionHoldMigration(t *testing.T) {
	hold, err := FS.ReadFile("197_video_subscription_frozen_quota.sql")
	require.NoError(t, err)
	sql := string(hold)
	require.Contains(t, sql, "frozen_quota DECIMAL(20,10) NOT NULL DEFAULT 0")
	require.Contains(t, sql, "CHECK (frozen_quota >= 0)")
}

func TestUnifiedVideoBillingAmountsUseExistingLedgerScale(t *testing.T) {
	original, err := FS.ReadFile("198_unified_video_billing_precision.sql")
	require.NoError(t, err)
	sum := sha256.Sum256(original)
	require.Equal(t, "f79e36de138d08571f286cb1a992f635b46818d324e01875df2a11940f8afff8", hex.EncodeToString(sum[:]))

	precision, err := FS.ReadFile("199_restore_video_billing_ledger_scale.sql")
	require.NoError(t, err)
	sql := string(precision)
	require.Contains(t, sql, "ALTER TABLE users")
	require.Contains(t, sql, "ALTER COLUMN balance TYPE DECIMAL(20,8)")
	require.Contains(t, sql, "ALTER COLUMN frozen_balance TYPE DECIMAL(20,8)")
	require.Contains(t, sql, "ALTER COLUMN estimated_amount TYPE DECIMAL(20,8)")
	require.Contains(t, sql, "ALTER COLUMN frozen_amount TYPE DECIMAL(20,8)")
	require.Contains(t, sql, "ALTER COLUMN settled_amount TYPE DECIMAL(20,8)")
}

func TestUnifiedVideoIdempotencyScopeMigration(t *testing.T) {
	migration, err := FS.ReadFile("200_video_idempotency_scope.sql")
	require.NoError(t, err)
	sql := string(migration)
	require.Contains(t, sql, "LOCK TABLE video_tasks IN SHARE MODE")
	require.Contains(t, sql, "GROUP BY user_id, api_key_id, idempotency_key_hash")
	require.Contains(t, sql, "migration 200 cannot enforce video idempotency scope")
	require.Contains(t, sql, "DROP INDEX idx_video_tasks_idempotency")
	require.Contains(t, sql, "ON video_tasks (user_id, api_key_id, idempotency_key_hash)")
	require.Contains(t, sql, "WHERE idempotency_key_hash <> ''")
	require.NotContains(t, sql, "DELETE FROM video_tasks")
	require.NotContains(t, sql, "UPDATE video_tasks")
}

func TestKlingVideoPollRouteHintMigrationRetainsOnlyStrictDiscriminator(t *testing.T) {
	migration, err := FS.ReadFile("201_kling_video_poll_route_hint.sql")
	require.NoError(t, err)
	sql := string(migration)
	require.Contains(t, sql, "DROP CONSTRAINT video_tasks_upstream_payload_cleared")
	require.Contains(t, sql, "provider = 'kling'")
	require.Contains(t, sql, "request_payload = jsonb_build_object")
	require.Contains(t, sql, "'text2video', 'image2video'")
	require.Contains(t, sql, "'video-extend'")
	require.Contains(t, sql, "COMMENT ON COLUMN video_tasks.request_payload")
	require.Contains(t, sql, "submitting")
	require.Contains(t, sql, "accepted Kling")
	require.Contains(t, sql, "provider_task_kind")
	require.Contains(t, sql, "other accepted tasks store NULL")
	require.NotContains(t, sql, "prompt")
	require.NotContains(t, sql, "video_id")
}
