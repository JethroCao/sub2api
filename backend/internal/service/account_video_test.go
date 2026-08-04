package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
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

func TestValidateVideoAccountConfigMapsInvalidInputToBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		accountType string
		extra       map[string]any
		credentials map[string]any
		wantReason  string
	}{
		{"oauth type", AccountTypeOAuth, map[string]any{"video_provider": VideoProviderSeedance}, map[string]any{"api_key": "ark-key"}, "VIDEO_ACCOUNT_TYPE_INVALID"},
		{"missing provider", AccountTypeAPIKey, map[string]any{}, map[string]any{"api_key": "ark-key"}, "VIDEO_PROVIDER_INVALID"},
		{"missing seedance key", AccountTypeAPIKey, map[string]any{"video_provider": VideoProviderSeedance}, map[string]any{}, "VIDEO_SEEDANCE_API_KEY_REQUIRED"},
		{"missing kling key pair", AccountTypeAPIKey, map[string]any{"video_provider": VideoProviderKling}, map[string]any{"access_key": "ak"}, "VIDEO_KLING_CREDENTIALS_REQUIRED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVideoAccountConfig(PlatformVideo, tt.accountType, tt.extra, tt.credentials)
			require.Error(t, err)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			require.True(t, response.ErrorFrom(ctx, err))
			require.Equal(t, http.StatusBadRequest, recorder.Code)

			var body response.Response
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Equal(t, tt.wantReason, body.Reason)
		})
	}
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
