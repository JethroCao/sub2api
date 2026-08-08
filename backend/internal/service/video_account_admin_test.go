package service

import (
	"encoding/json"
	"fmt"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestNormalizeVideoAccountAdminCreateStoresOnlyAllowlistedKeys(t *testing.T) {
	credentials, extra, err := NormalizeVideoAccountAdminCreate(
		PlatformVideo,
		map[string]any{
			"api_key":  "ark-key",
			"base_url": "https://ark.example.com/",
		},
		map[string]any{
			"video_provider":              VideoProviderSeedance,
			"model_mapping":               map[string]any{"seedance-2.0": "ep-seedance"},
			"video_disabled_capabilities": []any{"audio"},
		},
	)

	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"api_key":  "ark-key",
		"base_url": "https://ark.example.com",
	}, credentials)
	require.Equal(t, map[string]any{
		"video_provider":              VideoProviderSeedance,
		"model_mapping":               map[string]any{"seedance-2.0": "ep-seedance"},
		"video_disabled_capabilities": []string{"audio"},
	}, extra)
}

func TestNormalizeVideoAccountAdminCreateLeavesNonVideoMapsCompatible(t *testing.T) {
	credentials := map[string]any{"api_key": "secret", "legacy_option": true}
	extra := map[string]any{"legacy_extra": "kept"}

	gotCredentials, gotExtra, err := NormalizeVideoAccountAdminCreate(PlatformAnthropic, credentials, extra)

	require.NoError(t, err)
	require.Equal(t, credentials, gotCredentials)
	require.Equal(t, extra, gotExtra)
}

func TestNormalizeVideoAccountAdminCreateRejectsUnapprovedKeysAndInvalidHTTPS(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]any
		extra       map[string]any
		wantReason  string
	}{
		{
			name:        "unknown credential",
			credentials: map[string]any{"api_key": "ark-key", "region": "cn"},
			extra:       map[string]any{"video_provider": VideoProviderSeedance},
			wantReason:  "VIDEO_CREDENTIAL_KEY_INVALID",
		},
		{
			name:        "unknown extra",
			credentials: map[string]any{"api_key": "ark-key"},
			extra:       map[string]any{"video_provider": VideoProviderSeedance, "provider_state": "trusted"},
			wantReason:  "VIDEO_EXTRA_KEY_INVALID",
		},
		{
			name:        "http base url",
			credentials: map[string]any{"api_key": "ark-key", "base_url": "http://ark.example.com"},
			extra:       map[string]any{"video_provider": VideoProviderSeedance},
			wantReason:  "VIDEO_BASE_URL_INVALID",
		},
		{
			name:        "unknown capability",
			credentials: map[string]any{"api_key": "ark-key"},
			extra: map[string]any{
				"video_provider":              VideoProviderSeedance,
				"video_disabled_capabilities": []any{"telepathy"},
			},
			wantReason: "VIDEO_CAPABILITY_INVALID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NormalizeVideoAccountAdminCreate(PlatformVideo, tt.credentials, tt.extra)
			require.Error(t, err)
			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
		})
	}
}

func TestNormalizeVideoAccountAdminUpdateUsesFinalMergedConfig(t *testing.T) {
	existing := &Account{
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key":       "old-key",
			"base_url":      "https://old.example.com",
			"model_mapping": map[string]any{"legacy": "legacy-upstream"},
		},
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
		},
	}

	credentials, extra, err := NormalizeVideoAccountAdminUpdate(existing, map[string]any{
		"base_url": "https://new.example.com/",
	}, map[string]any{
		"video_disabled_capabilities": []any{"audio", "reference_videos"},
	}, "")

	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"api_key":  "old-key",
		"base_url": "https://new.example.com",
	}, credentials)
	require.Equal(t, map[string]any{
		"video_provider":              VideoProviderSeedance,
		"model_mapping":               map[string]any{"legacy": "legacy-upstream"},
		"video_disabled_capabilities": []string{"audio", "reference_videos"},
	}, extra)
}

func TestNormalizeVideoAccountAdminUpdateSecretClearingRequiresExplicitInactive(t *testing.T) {
	existing := &Account{
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_key": "access",
			"secret_key": "secret",
		},
		Extra: map[string]any{"video_provider": VideoProviderKling},
	}

	_, _, err := NormalizeVideoAccountAdminUpdate(existing, map[string]any{"secret_key": ""}, nil, "")
	require.Error(t, err)
	require.Equal(t, "VIDEO_SECRET_CLEAR_REQUIRES_DISABLE", infraerrors.Reason(err))

	credentials, extra, err := NormalizeVideoAccountAdminUpdate(
		existing,
		map[string]any{"access_key": nil, "secret_key": ""},
		nil,
		"inactive",
	)
	require.NoError(t, err)
	require.NotContains(t, credentials, "access_key")
	require.NotContains(t, credentials, "secret_key")
	require.Equal(t, VideoProviderKling, extra["video_provider"])
}

func TestBuildVideoAccountAdminMetadataNeverExposesSecrets(t *testing.T) {
	account := &Account{
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":    "api-secret",
			"access_key": "access-secret",
			"secret_key": "secret-secret",
			"base_url":   "https://example.com",
		},
		Extra: map[string]any{
			"video_provider":              VideoProviderSeedance,
			"video_disabled_capabilities": []any{"audio"},
		},
	}

	got := BuildVideoAccountAdminMetadata(account)
	require.Equal(t, VideoProviderSeedance, got.Provider)
	require.Contains(t, got.CapabilityTags, "generation")
	require.Contains(t, got.CapabilityTags, "reference_videos")
	require.NotContains(t, got.CapabilityTags, "audio")
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprint(got), "secret-secret")
	require.NotContains(t, string(raw), "api-secret")
	require.NotContains(t, string(raw), "access-secret")
}

func TestVideoAccountModelMappingReadsNewExtraAndLegacyCredentials(t *testing.T) {
	newAccount := &Account{
		Platform:    PlatformVideo,
		Credentials: map[string]any{"api_key": "key"},
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
			"model_mapping":  map[string]any{"seedance-2.0": "ep-new"},
		},
	}
	legacyAccount := &Account{
		Platform: PlatformVideo,
		Credentials: map[string]any{
			"api_key":       "key",
			"model_mapping": map[string]any{"seedance-2.0": "ep-legacy"},
		},
		Extra: map[string]any{"video_provider": VideoProviderSeedance},
	}

	require.Equal(t, "ep-new", newAccount.GetMappedModel("seedance-2.0"))
	require.Equal(t, "ep-legacy", legacyAccount.GetMappedModel("seedance-2.0"))
}

func TestValidateVideoAccountCapabilityOverridesRejectsDisabledRequestFeatures(t *testing.T) {
	withAudio := true
	account := &Account{
		Platform: PlatformVideo,
		Extra: map[string]any{
			"video_provider":              VideoProviderSeedance,
			"video_disabled_capabilities": []any{"audio", "reference_videos"},
		},
	}

	err := ValidateVideoAccountCapabilityOverrides(account, CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model:     "seedance-2.0",
		Prompt:    "waves",
		Audio:     &withAudio,
	})
	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)

	err = ValidateVideoAccountCapabilityOverrides(account, CanonicalVideoRequest{
		Operation:       VideoOperationGeneration,
		Model:           "seedance-2.0",
		Prompt:          "waves",
		ReferenceVideos: []VideoAsset{{URL: "https://example.com/ref.mp4"}},
	})
	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)

	err = ValidateVideoAccountCapabilityOverrides(account, CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model:     "seedance-2.0",
		Prompt:    "waves",
	})
	require.NoError(t, err)
}
