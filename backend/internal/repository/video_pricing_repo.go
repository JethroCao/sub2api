package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type videoPricingRepository struct {
	db *sql.DB
}

func NewVideoPricingRepository(db *sql.DB) service.VideoPricingRepository {
	return &videoPricingRepository{db: db}
}

// ListMatching returns every enabled rule that can match the request. The SQL
// ordering mirrors service specificity so callers can inspect deterministic
// candidates while the service remains the sole authority for quote creation.
func (r *videoPricingRepository) ListMatching(ctx context.Context, query service.VideoPricingQuery) ([]service.VideoPricingRule, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("video pricing repository db is nil")
	}

	audioMode := "without_audio"
	if query.Audio {
		audioMode = "with_audio"
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, group_id, external_model, operation, resolution, audio_mode,
		       unit, unit_price, upstream_unit_cost, enabled
		FROM video_pricing_rules
		WHERE group_id = $1
		  AND external_model = $2
		  AND operation = $3
		  AND enabled = TRUE
		  AND (resolution = '*' OR resolution = $4)
		  AND (audio_mode = 'any' OR audio_mode = $5)
		ORDER BY
		  CASE WHEN resolution = $4 THEN 0 ELSE 1 END,
		  CASE WHEN audio_mode = $5 THEN 0 ELSE 1 END,
		  id ASC`,
		query.GroupID, query.ExternalModel, query.Operation, query.Resolution, audioMode)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	rules := make([]service.VideoPricingRule, 0)
	for rows.Next() {
		var rule service.VideoPricingRule
		var upstreamUnitCost sql.NullFloat64
		if err := rows.Scan(
			&rule.ID, &rule.GroupID, &rule.ExternalModel, &rule.Operation, &rule.Resolution, &rule.AudioMode,
			&rule.Unit, &rule.UnitPrice, &upstreamUnitCost, &rule.Enabled,
		); err != nil {
			return nil, err
		}
		if upstreamUnitCost.Valid {
			value := upstreamUnitCost.Float64
			rule.UpstreamUnitCost = &value
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}
