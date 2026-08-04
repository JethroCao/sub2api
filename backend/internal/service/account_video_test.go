package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateVideoAccountConfig(t *testing.T) {
	tests := []struct {
		name        string
		accountType string
		extra       map[string]any
		credentials map[string]any
		wantErr     bool
	}{
		{"seedance api key", AccountTypeAPIKey, map[string]any{"video_provider": "seedance"}, map[string]any{"api_key": "ark-key"}, false},
		{"kling key pair", AccountTypeAPIKey, map[string]any{"video_provider": "kling"}, map[string]any{"access_key": "ak", "secret_key": "sk"}, false},
		{"missing provider", AccountTypeAPIKey, map[string]any{}, map[string]any{"api_key": "x"}, true},
		{"oauth rejected", AccountTypeOAuth, map[string]any{"video_provider": "seedance"}, map[string]any{"api_key": "x"}, true},
		{"unknown provider", AccountTypeAPIKey, map[string]any{"video_provider": "other"}, map[string]any{"api_key": "x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVideoAccountConfig(PlatformVideo, tt.accountType, tt.extra, tt.credentials)
			require.Equal(t, tt.wantErr, err != nil)
		})
	}
}

func TestAllowedQuotaPlatformsIncludesVideo(t *testing.T) {
	require.True(t, IsAllowedQuotaPlatform(PlatformVideo))
}

func TestAccountVideoProviderNormalizesOnlyVideoAccounts(t *testing.T) {
	require.Equal(t, VideoProviderSeedance, (&Account{
		Platform: PlatformVideo,
		Extra:    map[string]any{"video_provider": " Seedance "},
	}).VideoProvider())
	require.Empty(t, (&Account{
		Platform: PlatformOpenAI,
		Extra:    map[string]any{"video_provider": VideoProviderSeedance},
	}).VideoProvider())
}
