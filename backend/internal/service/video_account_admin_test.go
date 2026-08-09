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

func TestNormalizeVideoAccountAdminRejectsMissingOrClearedModelMapping(t *testing.T) {
	_, _, err := NormalizeVideoAccountAdminCreate(
		PlatformVideo,
		map[string]any{"api_key": "ark-key"},
		map[string]any{"video_provider": VideoProviderSeedance},
	)
	require.Equal(t, "VIDEO_MODEL_MAPPING_REQUIRED", infraerrors.Reason(err))

	_, _, err = NormalizeVideoAccountAdminCreate(
		PlatformVideo,
		map[string]any{"api_key": "ark-key"},
		map[string]any{
			VideoProviderExtraKey:     VideoProviderSeedance,
			VideoModelMappingExtraKey: map[string]any{"seedance-*": "endpoint-id"},
		},
	)
	require.Equal(t, "VIDEO_MODEL_MAPPING_INVALID", infraerrors.Reason(err))

	_, _, err = NormalizeVideoAccountAdminCreate(
		PlatformVideo,
		map[string]any{"api_key": "ark-key"},
		map[string]any{
			VideoProviderExtraKey:     VideoProviderSeedance,
			VideoModelMappingExtraKey: map[string]any{"seedance-2.0": "seedance-2.0"},
		},
	)
	require.NoError(t, err)

	err = ValidateVideoAccountAdminFinalConfig(
		PlatformVideo,
		AccountTypeAPIKey,
		StatusActive,
		map[string]any{"video_provider": VideoProviderSeedance},
		map[string]any{"api_key": "ark-key"},
	)
	require.Equal(t, "VIDEO_MODEL_MAPPING_REQUIRED", infraerrors.Reason(err))

	account := &Account{
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key": "ark-key",
		},
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
			"model_mapping":  map[string]any{"seedance-2.0": "endpoint-id"},
		},
	}
	_, _, err = NormalizeVideoAccountAdminUpdate(
		account,
		nil,
		map[string]any{"model_mapping": map[string]any{}},
		StatusActive,
	)
	require.Equal(t, "VIDEO_MODEL_MAPPING_REQUIRED", infraerrors.Reason(err))
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
			extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			wantReason:  "VIDEO_CREDENTIAL_KEY_INVALID",
		},
		{
			name:        "unknown extra",
			credentials: map[string]any{"api_key": "ark-key"},
			extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}, "provider_state": "trusted"},
			wantReason:  "VIDEO_EXTRA_KEY_INVALID",
		},
		{
			name:        "http base url",
			credentials: map[string]any{"api_key": "ark-key", "base_url": "http://ark.example.com"},
			extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			wantReason:  "VIDEO_BASE_URL_INVALID",
		},
		{
			name:        "unknown capability",
			credentials: map[string]any{"api_key": "ark-key"},
			extra: map[string]any{
				"video_provider":              VideoProviderSeedance,
				"model_mapping":               map[string]any{"seedance-2.0": "endpoint-id"},
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

func TestNormalizeVideoAccountAdminRejectsProviderUnsafeBaseURLsWithoutEchoingSecrets(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		credentials map[string]any
		mapping     map[string]any
	}{
		{name: "seedance userinfo", provider: VideoProviderSeedance, credentials: map[string]any{"api_key": "ark-key", "base_url": "https://admin:base-secret@ark.example.com"}, mapping: map[string]any{"seedance-2.0": "endpoint"}},
		{name: "seedance query", provider: VideoProviderSeedance, credentials: map[string]any{"api_key": "ark-key", "base_url": "https://ark.example.com?token=base-secret"}, mapping: map[string]any{"seedance-2.0": "endpoint"}},
		{name: "seedance fragment", provider: VideoProviderSeedance, credentials: map[string]any{"api_key": "ark-key", "base_url": "https://ark.example.com/#base-secret"}, mapping: map[string]any{"seedance-2.0": "endpoint"}},
		{name: "seedance unverified path", provider: VideoProviderSeedance, credentials: map[string]any{"api_key": "ark-key", "base_url": "https://ark.example.com/unverified"}, mapping: map[string]any{"seedance-2.0": "endpoint"}},
		{name: "kling userinfo", provider: VideoProviderKling, credentials: map[string]any{"access_key": "access", "secret_key": "secret", "base_url": "https://admin:base-secret@kling.example.com"}, mapping: map[string]any{"kling-3.0": "kling-v3"}},
		{name: "kling query", provider: VideoProviderKling, credentials: map[string]any{"access_key": "access", "secret_key": "secret", "base_url": "https://kling.example.com?token=base-secret"}, mapping: map[string]any{"kling-3.0": "kling-v3"}},
		{name: "kling fragment", provider: VideoProviderKling, credentials: map[string]any{"access_key": "access", "secret_key": "secret", "base_url": "https://kling.example.com/#base-secret"}, mapping: map[string]any{"kling-3.0": "kling-v3"}},
		{name: "kling non-root path", provider: VideoProviderKling, credentials: map[string]any{"access_key": "access", "secret_key": "secret", "base_url": "https://kling.example.com/v1"}, mapping: map[string]any{"kling-3.0": "kling-v3"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NormalizeVideoAccountAdminCreate(PlatformVideo, tt.credentials, map[string]any{
				VideoProviderExtraKey:     tt.provider,
				VideoModelMappingExtraKey: tt.mapping,
			})

			require.Equal(t, "VIDEO_BASE_URL_INVALID", infraerrors.Reason(err))
			require.NotContains(t, fmt.Sprint(err), "base-secret")
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

func TestNormalizeVideoAccountAdminUpdateExplicitEmptyBaseURLClearsStoredValue(t *testing.T) {
	existing := &Account{
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key":  "old-key",
			"base_url": "https://old.example.com",
		},
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
			"model_mapping":  map[string]any{"seedance-2.0": "endpoint-id"},
		},
	}

	credentials, _, err := NormalizeVideoAccountAdminUpdate(
		existing,
		map[string]any{"base_url": ""},
		nil,
		"",
	)

	require.NoError(t, err)
	require.Equal(t, map[string]any{"api_key": "old-key"}, credentials)
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
		Extra: map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
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
	catalog := VideoCapabilityCatalog{
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-2.0"): {
			VideoOperationGeneration: {Text: true, Audio: true},
		},
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-1.0"): {
			VideoOperationGeneration: {FirstFrame: true, ReferenceImages: true},
		},
	}
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
			"model_mapping":               map[string]any{"seedance-2.0": "endpoint-id"},
			"video_disabled_capabilities": []any{"audio"},
		},
	}

	got := BuildVideoAccountAdminMetadata(account, catalog)
	require.Equal(t, VideoProviderSeedance, got.Provider)
	require.Contains(t, got.CapabilityTags, "generation")
	require.Contains(t, got.CapabilityTags, "text")
	require.NotContains(t, got.CapabilityTags, "audio")
	require.NotContains(t, got.CapabilityTags, "first_frame")
	require.NotContains(t, got.CapabilityTags, "reference_images")
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprint(got), "secret-secret")
	require.NotContains(t, string(raw), "api-secret")
	require.NotContains(t, string(raw), "access-secret")

	account.Extra[VideoDisabledCapabilitiesExtraKey] = []any{"telepathy"}
	corrupt := BuildVideoAccountAdminMetadata(account, catalog)
	require.Equal(t, VideoProviderSeedance, corrupt.Provider)
	require.Empty(t, corrupt.CapabilityTags)

	account.Extra[VideoDisabledCapabilitiesExtraKey] = []any{}
	delete(account.Extra, VideoModelMappingExtraKey)
	missingMapping := BuildVideoAccountAdminMetadata(account, catalog)
	require.Equal(t, VideoProviderSeedance, missingMapping.Provider)
	require.Empty(t, missingMapping.CapabilityTags)

	account.Extra[VideoModelMappingExtraKey] = map[string]any{"seedance-unknown": "endpoint"}
	unknownMapping := BuildVideoAccountAdminMetadata(account, catalog)
	require.Equal(t, VideoProviderSeedance, unknownMapping.Provider)
	require.Empty(t, unknownMapping.CapabilityTags)
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
	catalog := VideoCapabilityCatalog{
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-2.0"): {
			VideoOperationGeneration: {Text: true},
		},
	}
	require.Contains(t, BuildVideoAccountAdminMetadata(legacyAccount, catalog).CapabilityTags, "generation")
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

func TestValidateVideoAccountCapabilityOverridesRejectsCorruptPersistedCapabilities(t *testing.T) {
	for _, raw := range []any{
		"audio",
		[]any{"telepathy"},
		[]any{42},
	} {
		account := &Account{
			Platform: PlatformVideo,
			Extra: map[string]any{
				VideoProviderExtraKey:             VideoProviderSeedance,
				VideoDisabledCapabilitiesExtraKey: raw,
			},
		}

		err := ValidateVideoAccountCapabilityOverrides(account, CanonicalVideoRequest{
			Operation: VideoOperationGeneration,
			Model:     "seedance-2.0",
			Prompt:    "waves",
		})

		require.Equal(t, "VIDEO_CAPABILITY_INVALID", infraerrors.Reason(err))
	}
}
