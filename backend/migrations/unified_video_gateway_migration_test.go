package migrations

import (
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
}
