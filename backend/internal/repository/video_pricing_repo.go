package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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
	resolution := strings.ToLower(strings.TrimSpace(query.Resolution))
	if resolution == "" {
		resolution = "480p"
	}
	legacyEligible := strings.HasPrefix(strings.ToLower(strings.TrimSpace(query.ExternalModel)), "grok-") &&
		query.Operation == string(service.VideoOperationGeneration) &&
		(resolution == "480p" || resolution == "720p" || resolution == "1080p")
	rows, err := r.db.QueryContext(ctx, `
WITH group_has_rules AS (
    SELECT 1 FROM video_pricing_rules WHERE group_id=$1 LIMIT 1
), explicit_rules AS (
    SELECT id, group_id, external_model, operation, resolution, audio_mode,
           unit, unit_price, upstream_unit_cost, enabled, FALSE AS legacy
    FROM video_pricing_rules
    WHERE group_id=$1 AND external_model=$2 AND operation=$3 AND enabled=TRUE
      AND (resolution='*' OR resolution=$4)
      AND (audio_mode='any' OR audio_mode=$5)
), legacy_rule AS (
    SELECT 0::BIGINT AS id, g.id AS group_id, $2::TEXT AS external_model,
           $3::TEXT AS operation, $4::TEXT AS resolution, 'any'::TEXT AS audio_mode,
           'per_output_second'::TEXT AS unit,
           CASE $4 WHEN '720p' THEN g.video_price_720p WHEN '1080p' THEN g.video_price_1080p ELSE g.video_price_480p END AS unit_price,
           NULL::NUMERIC AS upstream_unit_cost, TRUE AS enabled, TRUE AS legacy
    FROM groups g
    WHERE g.id=$1 AND g.deleted_at IS NULL AND $6
      AND NOT EXISTS (SELECT 1 FROM group_has_rules)
      AND CASE $4 WHEN '720p' THEN g.video_price_720p WHEN '1080p' THEN g.video_price_1080p ELSE g.video_price_480p END IS NOT NULL
), candidates AS (
    SELECT * FROM explicit_rules
    UNION ALL
    SELECT * FROM legacy_rule
)
SELECT * FROM candidates
ORDER BY CASE WHEN resolution=$4 THEN 0 ELSE 1 END,
         CASE WHEN audio_mode=$5 THEN 0 ELSE 1 END, id`,
		query.GroupID, query.ExternalModel, query.Operation, resolution, audioMode, legacyEligible)
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
			&rule.Unit, &rule.UnitPrice, &upstreamUnitCost, &rule.Enabled, &rule.Legacy,
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
