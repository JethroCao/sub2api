package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoPricingRepositoryResolvesExplicitCutoffAndLegacyInOneSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`(?s)WITH group_has_rules AS.*explicit_rules AS.*legacy_rule AS.*UNION ALL`).
		WithArgs(int64(7), "grok-imagine-video", "generation", "720p", "without_audio", true).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "group_id", "external_model", "operation", "resolution", "audio_mode",
			"unit", "unit_price", "upstream_unit_cost", "enabled", "legacy",
		}).AddRow(0, 7, "grok-imagine-video", "generation", "720p", "any", "per_output_second", 0.37, nil, true, true))

	rules, err := NewVideoPricingRepository(db).ListMatching(context.Background(), service.VideoPricingQuery{
		GroupID: 7, ExternalModel: "grok-imagine-video", Operation: "generation", Resolution: "720p",
	})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.True(t, rules[0].Legacy)
	require.NoError(t, mock.ExpectationsWereMet())
}
