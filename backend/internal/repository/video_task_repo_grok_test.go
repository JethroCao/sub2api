package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskRepositoryValidateCreateParamsAllowsOnlySupportedDurableRoutes(t *testing.T) {
	nextPollAt := time.Now().UTC().Add(time.Minute)
	base := service.CreateVideoTaskParams{
		UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 0,
		Operation: "generation", ExternalModel: "model", UpstreamModel: "",
		RequestHash: strings.Repeat("a", 64), PricingUnit: "per_request",
		Currency: "USD", BillingMode: "balance", BillingStatus: "held", NextPollAt: &nextPollAt,
	}
	tests := []struct {
		name     string
		platform string
		provider string
		wantErr  bool
	}{
		{name: "grok durable", platform: service.PlatformGrok, provider: service.PlatformGrok},
		{name: "seedance durable", platform: service.PlatformVideo, provider: service.VideoProviderSeedance},
		{name: "kling schema remains valid but runtime gated", platform: service.PlatformVideo, provider: service.VideoProviderKling},
		{name: "grok seedance mismatch", platform: service.PlatformGrok, provider: service.VideoProviderSeedance, wantErr: true},
		{name: "video grok mismatch", platform: service.PlatformVideo, provider: service.PlatformGrok, wantErr: true},
		{name: "unknown provider", platform: service.PlatformVideo, provider: "other", wantErr: true},
		{name: "unknown platform", platform: "other", provider: service.PlatformGrok, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := base
			params.Platform, params.Provider = tt.platform, tt.provider
			err := validateCreateVideoTaskParams(params)
			if tt.wantErr {
				require.ErrorIs(t, err, service.ErrVideoTaskInvalidRequest)
				return
			}
			require.NoError(t, err)
		})
	}
}
