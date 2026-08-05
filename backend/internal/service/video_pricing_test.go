package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeVideoPricingRepo struct {
	rules []VideoPricingRule
	err   error
}

func (r *fakeVideoPricingRepo) ListMatching(_ context.Context, _ VideoPricingQuery) ([]VideoPricingRule, error) {
	return r.rules, r.err
}

func TestVideoPricingResolveSpecificityAndQuote(t *testing.T) {
	upstreamCost := 0.08
	repo := &fakeVideoPricingRepo{rules: []VideoPricingRule{
		{ID: 1, GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_output_second", UnitPrice: 0.10},
		{ID: 2, GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "1080p", AudioMode: "with_audio", Unit: "per_output_second", UnitPrice: 0.25, UpstreamUnitCost: &upstreamCost},
	}}

	quote, err := NewVideoPricingService(repo).Quote(context.Background(), VideoPricingQuery{
		GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "1080p", Audio: true, DurationSeconds: 6,
	})

	require.NoError(t, err)
	require.EqualValues(t, 2, quote.RuleID)
	require.Equal(t, int64(7), quote.GroupID)
	require.Equal(t, "seedance-2.0", quote.ExternalModel)
	require.Equal(t, "generation", quote.Operation)
	require.Equal(t, "1080p", quote.Resolution)
	require.Equal(t, "with_audio", quote.AudioMode)
	require.Equal(t, "per_output_second", quote.Unit)
	require.InDelta(t, 0.25, quote.UnitPrice, 1e-9)
	require.NotNil(t, quote.UpstreamUnitCost)
	require.InDelta(t, 0.08, *quote.UpstreamUnitCost, 1e-9)
	require.InDelta(t, 6, quote.Units, 1e-9)
	require.InDelta(t, 1.50, quote.HoldAmount, 1e-9)
}

func TestVideoPricingMissingRuleFailsClosed(t *testing.T) {
	_, err := NewVideoPricingService(&fakeVideoPricingRepo{}).Quote(context.Background(), VideoPricingQuery{
		GroupID: 7, ExternalModel: "kling-3.0", Operation: "edit",
	})
	require.ErrorIs(t, err, ErrVideoPricingUnavailable)
}

func TestVideoPricingPerRequestQuoteUsesOneUnit(t *testing.T) {
	repo := &fakeVideoPricingRepo{rules: []VideoPricingRule{{
		ID: 3, GroupID: 7, ExternalModel: "kling-3.0", Operation: "edit", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: 1.25,
	}}}

	quote, err := NewVideoPricingService(repo).Quote(context.Background(), VideoPricingQuery{
		GroupID: 7, ExternalModel: "kling-3.0", Operation: "edit", DurationSeconds: 0,
	})

	require.NoError(t, err)
	require.InDelta(t, 1, quote.Units, 1e-9)
	require.InDelta(t, 1.25, quote.HoldAmount, 1e-9)
}

func TestVideoPricingRejectsInvalidPerSecondDurationAndNegativePrices(t *testing.T) {
	tests := []struct {
		name  string
		rule  VideoPricingRule
		query VideoPricingQuery
	}{
		{
			name:  "zero per-second duration",
			rule:  VideoPricingRule{ID: 4, GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_output_second", UnitPrice: 0.1},
			query: VideoPricingQuery{GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", DurationSeconds: 0},
		},
		{
			name:  "negative unit price",
			rule:  VideoPricingRule{ID: 5, GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: -0.1},
			query: VideoPricingQuery{GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewVideoPricingService(&fakeVideoPricingRepo{rules: []VideoPricingRule{tt.rule}}).Quote(context.Background(), tt.query)
			require.ErrorIs(t, err, ErrVideoPricingInvalid)
		})
	}
}
