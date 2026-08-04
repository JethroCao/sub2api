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
