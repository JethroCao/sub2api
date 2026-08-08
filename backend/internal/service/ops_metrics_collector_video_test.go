package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVideoMetricsAggregationCoversOperationalAndFinancialSignals(t *testing.T) {
	query := strings.ToLower(videoOpsMetricsAggregationSQL)
	for _, signal := range []string{
		"submission_latency", "provider_queue", "completion", "rate_limit",
		"unknown_count", "unknown_max_age", "expired_lease", "pending_settlement",
		"failed_refund", "revenue", "upstream_cost", "margin",
	} {
		require.Contains(t, query, signal)
	}
}

func TestVideoMetricDimensionsExcludeUnboundedRequestData(t *testing.T) {
	query := strings.ToLower(videoOpsMetricsAggregationSQL)
	require.Contains(t, query, "with bounded")
	require.Contains(t, query, "external_model = any($3::text[])")
	require.NotContains(t, query, "coalesce(upstream_unit_cost")
	for _, forbidden := range []string{"request_id", "user_id", "api_key_id", "last_error_message", "request_payload", "result_url"} {
		require.NotContains(t, query, forbidden)
	}

	dimensions := normalizeVideoMetricDimensions(
		"seedance", "attacker-controlled-model", "generation", 7,
		map[string]struct{}{"seedance-2.0": {}},
	)
	require.Equal(t, "seedance", dimensions.Provider)
	require.Equal(t, "other", dimensions.Model)
	require.Equal(t, "generation", dimensions.Operation)
	require.Equal(t, int64(7), dimensions.GroupID)
}
