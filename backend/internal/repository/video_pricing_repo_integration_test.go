//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoPricingRepositoryReturnsEnabledMatchingRules(t *testing.T) {
	ctx := context.Background()
	groupID := createVideoPricingTestGroup(t)
	defer deleteVideoPricingTestGroup(t, groupID)

	wildcardID := createVideoPricingTestRule(t, groupID, "seedance-2.0", "generation", "*", "any", "per_output_second", 0.10, true)
	exactResolutionID := createVideoPricingTestRule(t, groupID, "seedance-2.0", "generation", "1080p", "any", "per_output_second", 0.25, true)
	_ = createVideoPricingTestRule(t, groupID, "seedance-2.0", "generation", "1080p", "with_audio", "per_output_second", 0.75, false)

	rules, err := NewVideoPricingRepository(integrationDB).ListMatching(ctx, service.VideoPricingQuery{
		GroupID: groupID, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "1080p", Audio: true,
	})

	require.NoError(t, err)
	require.Len(t, rules, 2)
	require.Equal(t, exactResolutionID, rules[0].ID)
	require.Equal(t, wildcardID, rules[1].ID)
}

func TestVideoPricingRepositoryRejectsDuplicateDimensions(t *testing.T) {
	groupID := createVideoPricingTestGroup(t)
	defer deleteVideoPricingTestGroup(t, groupID)

	createVideoPricingTestRule(t, groupID, "kling-3.0", "edit", "*", "any", "per_request", 1.25, true)
	_, err := integrationDB.ExecContext(context.Background(), `
		INSERT INTO video_pricing_rules (
			group_id, external_model, operation, resolution, audio_mode, unit, unit_price, enabled
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		groupID, "kling-3.0", "edit", "*", "any", "per_request", 2.50, true)
	require.Error(t, err)
}

func TestVideoPricingRepositoryLegacyGrokFallbackStopsAtFirstExplicitRule(t *testing.T) {
	ctx := context.Background()
	groupID := createVideoPricingTestGroup(t)
	defer deleteVideoPricingTestGroup(t, groupID)
	_, err := integrationDB.ExecContext(ctx, `UPDATE groups SET video_price_720p=0.37 WHERE id=$1`, groupID)
	require.NoError(t, err)
	repo := NewVideoPricingRepository(integrationDB)
	query := service.VideoPricingQuery{GroupID: groupID, ExternalModel: "grok-imagine-video", Operation: "generation", Resolution: "720p", DurationSeconds: 5}

	rules, err := repo.ListMatching(ctx, query)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.True(t, rules[0].Legacy)
	require.InDelta(t, 0.37, rules[0].UnitPrice, 1e-9)

	createVideoPricingTestRule(t, groupID, "seedance-2.0", "generation", "*", "any", "per_request", 1, false)
	rules, err = repo.ListMatching(ctx, query)
	require.NoError(t, err)
	require.Empty(t, rules, "any explicit group rule must disable the Grok legacy fallback")
}

func createVideoPricingTestGroup(t *testing.T) int64 {
	t.Helper()
	var groupID int64
	err := integrationDB.QueryRowContext(context.Background(), `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ($1, 'openai', 1, 'active', 'standard')
		RETURNING id`, fmt.Sprintf("video-pricing-%d", time.Now().UnixNano())).Scan(&groupID)
	require.NoError(t, err)
	return groupID
}

func deleteVideoPricingTestGroup(t *testing.T, groupID int64) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id = $1", groupID)
	require.NoError(t, err)
}

func createVideoPricingTestRule(t *testing.T, groupID int64, externalModel, operation, resolution, audioMode, unit string, unitPrice float64, enabled bool) int64 {
	t.Helper()
	var ruleID int64
	err := integrationDB.QueryRowContext(context.Background(), `
		INSERT INTO video_pricing_rules (
			group_id, external_model, operation, resolution, audio_mode, unit, unit_price, enabled
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`, groupID, externalModel, operation, resolution, audioMode, unit, unitPrice, enabled).Scan(&ruleID)
	require.NoError(t, err)
	return ruleID
}
