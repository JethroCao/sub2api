package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCreateVideoTaskParamsEnforcesBillingMetadataContract(t *testing.T) {
	positiveSubscriptionID := int64(7)
	zeroSubscriptionID := int64(0)
	tests := []struct {
		name           string
		mode           string
		subscriptionID *int64
		wantMode       string
		wantErr        error
	}{
		{name: "normalizes balance", mode: "  BALANCE  ", wantMode: "balance"},
		{name: "normalizes subscription", mode: " Subscription ", subscriptionID: &positiveSubscriptionID, wantMode: "subscription"},
		{name: "rejects arbitrary mode", mode: "credits", wantErr: service.ErrVideoTaskInvalidRequest},
		{name: "rejects balance subscription", mode: "balance", subscriptionID: &positiveSubscriptionID, wantErr: service.ErrVideoTaskInvalidRequest},
		{name: "rejects missing subscription", mode: "subscription", wantErr: service.ErrVideoTaskInvalidRequest},
		{name: "rejects zero subscription", mode: "subscription", subscriptionID: &zeroSubscriptionID, wantErr: service.ErrVideoTaskInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := service.NewMinimizedVideoPayload(map[string]any{"prompt": "safe"})
			require.NoError(t, err)
			params := service.CreateVideoTaskParams{
				UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4,
				Platform: service.PlatformVideo, Provider: service.VideoProviderSeedance,
				Operation: "generation", ExternalModel: "video-model", UpstreamModel: "upstream-model",
				RequestHash: strings.Repeat("a", 64), RequestPayload: payload,
				PricingUnit: "per_output_second", UnitPrice: 1, EstimatedUnits: 1,
				EstimatedAmount: 1, FrozenAmount: 1, Currency: "USD", BillingStatus: "pending",
			}
			params.BillingMode = tt.mode
			params.SubscriptionID = tt.subscriptionID
			got, err := normalizeCreateVideoTaskParams(params)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantMode, got.BillingMode)
		})
	}
}
