package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateVideoCapabilityRejectsBeforeBilling(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderSeedance: {
		VideoOperationGeneration: {Text: true, FirstFrame: true},
	}}
	req := CanonicalVideoRequest{Operation: VideoOperationExtension, Model: "seedance-2.0"}
	err := catalog.Validate(VideoProviderSeedance, req)
	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)
}

type videoProviderRegistryStub struct {
	name string
}

func (p videoProviderRegistryStub) Name() string { return p.name }
func (p videoProviderRegistryStub) Capabilities() VideoProviderCapabilities {
	return VideoProviderCapabilities{}
}
func (p videoProviderRegistryStub) Submit(context.Context, *Account, CanonicalVideoRequest, string) (VideoSubmitResult, error) {
	return VideoSubmitResult{}, nil
}
func (p videoProviderRegistryStub) RecoverSubmission(context.Context, *Account, VideoTask, string) (VideoSubmitResult, bool, error) {
	return VideoSubmitResult{}, false, nil
}
func (p videoProviderRegistryStub) Poll(context.Context, *Account, VideoTask) (VideoPollResult, error) {
	return VideoPollResult{}, nil
}
func (p videoProviderRegistryStub) OpenContent(context.Context, *Account, VideoTask) (io.ReadCloser, http.Header, int64, error) {
	return nil, nil, 0, nil
}

func TestVideoProviderRegistryRejectsDuplicateNames(t *testing.T) {
	_, err := NewVideoProviderRegistry(
		videoProviderRegistryStub{name: VideoProviderSeedance},
		videoProviderRegistryStub{name: VideoProviderSeedance},
	)
	require.ErrorIs(t, err, ErrVideoProviderDuplicate)
}

func TestVideoProviderRegistryRejectsInvalidProvidersAndResolvesExactNames(t *testing.T) {
	_, err := NewVideoProviderRegistry(videoProviderRegistryStub{name: " Seedance "})
	require.ErrorIs(t, err, ErrVideoProviderInvalid)

	_, err = NewVideoProviderRegistry(nil)
	require.ErrorIs(t, err, ErrVideoProviderInvalid)
	var typedNil *videoProviderRegistryStub
	require.NotPanics(t, func() {
		_, err = NewVideoProviderRegistry(typedNil)
	})
	require.ErrorIs(t, err, ErrVideoProviderInvalid)

	provider := videoProviderRegistryStub{name: VideoProviderSeedance}
	registry, err := NewVideoProviderRegistry(provider)
	require.NoError(t, err)
	got, ok := registry.Get(VideoProviderSeedance)
	require.True(t, ok)
	require.Equal(t, VideoProviderSeedance, got.Name())
	_, ok = registry.Get(" Seedance ")
	require.False(t, ok)
}

func TestVideoProviderErrorIsStableAndRedactsUpstreamDetails(t *testing.T) {
	raw := errors.New(`{"error":"invalid","authorization":"Bearer secret-token","api_key":"sk-provider-secret"}`)
	err := NewVideoProviderError(http.StatusTooManyRequests, "UPSTREAM_RATE_LIMIT", true, false, raw)

	require.Equal(t, http.StatusTooManyRequests, err.HTTPStatus)
	require.Equal(t, "upstream_rate_limit", err.Code)
	require.True(t, err.Retryable)
	require.False(t, err.Ambiguous)
	require.NotContains(t, err.Error(), "secret-token")
	require.NotContains(t, err.Error(), "sk-provider-secret")
	require.NotContains(t, err.Error(), raw.Error())
	require.False(t, strings.Contains(strings.ToLower(err.Error()), "authorization"))
}

func TestVideoProviderErrorFallsBackFromUnsafePublicValues(t *testing.T) {
	err := NewVideoProviderError(0, "api_key=sk-provider-secret", false, true, errors.New("raw upstream body"))
	require.Equal(t, http.StatusBadGateway, err.HTTPStatus)
	require.Equal(t, "upstream_error", err.Code)
	require.Equal(t, "video provider error: upstream_error", err.Error())
}

func TestValidateVideoCapabilityChecksDerivedMediaAndOutputRequirements(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderSeedance: {
		VideoOperationGeneration: {
			Text:               true,
			FirstFrame:         true,
			ReferenceImages:    true,
			Audio:              true,
			MinDurationSeconds: 2,
			MaxDurationSeconds: 10,
			Resolutions:        []string{"720p", "1080p"},
			AspectRatios:       []string{"16:9", "9:16"},
		},
	}}
	audio := true
	valid := CanonicalVideoRequest{
		Operation:       VideoOperationGeneration,
		Model:           "seedance-2.0",
		Prompt:          "animate",
		DurationSeconds: 6,
		Resolution:      "1080p",
		AspectRatio:     "16:9",
		FirstFrame:      []VideoAsset{{URL: "https://example.com/first.png"}},
		ReferenceImages: []VideoAsset{{URL: "https://example.com/reference.png"}},
		Audio:           &audio,
	}
	require.NoError(t, catalog.Validate(VideoProviderSeedance, valid))

	tests := []struct {
		name   string
		mutate func(*CanonicalVideoRequest)
	}{
		{name: "last frame", mutate: func(req *CanonicalVideoRequest) { req.LastFrame = []VideoAsset{{URL: "https://example.com/last.png"}} }},
		{name: "reference video", mutate: func(req *CanonicalVideoRequest) {
			req.ReferenceVideos = []VideoAsset{{URL: "https://example.com/reference.mp4"}}
		}},
		{name: "duration below minimum", mutate: func(req *CanonicalVideoRequest) { req.DurationSeconds = 1 }},
		{name: "duration above maximum", mutate: func(req *CanonicalVideoRequest) { req.DurationSeconds = 11 }},
		{name: "resolution", mutate: func(req *CanonicalVideoRequest) { req.Resolution = "480p" }},
		{name: "aspect ratio", mutate: func(req *CanonicalVideoRequest) { req.AspectRatio = "1:1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid
			tt.mutate(&req)
			require.ErrorIs(t, catalog.Validate(VideoProviderSeedance, req), ErrVideoUnsupportedCapability)
		})
	}
}

func TestValidateVideoCapabilityRequiresExplicitAdaptiveAspectRatioSupport(t *testing.T) {
	request := CanonicalVideoRequest{
		Operation:   VideoOperationGeneration,
		Model:       "model",
		Prompt:      "animate",
		AspectRatio: "adaptive",
	}
	unconstrained := VideoCapabilityCatalog{"provider": {
		VideoOperationGeneration: {Text: true},
	}}
	require.ErrorIs(t, unconstrained.Validate("provider", request), ErrVideoUnsupportedCapability)

	explicit := VideoCapabilityCatalog{"provider": {
		VideoOperationGeneration: {Text: true, AspectRatios: []string{"adaptive"}},
	}}
	require.NoError(t, explicit.Validate("provider", request))
}

func TestValidateVideoCapabilityRequiresTextOnlyForTextOnlyGeneration(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderSeedance: {
		VideoOperationGeneration: {FirstFrame: true},
	}}
	require.ErrorIs(t, catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model:     "m",
		Prompt:    "text only",
	}), ErrVideoUnsupportedCapability)
	require.NoError(t, catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{
		Operation:  VideoOperationGeneration,
		Model:      "m",
		Prompt:     "guide the animation",
		FirstFrame: []VideoAsset{{URL: "https://example.com/a.png"}},
	}))
}

func TestValidateVideoCapabilityTreatsEditAndExtensionSourceAsOperationInput(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderKling: {
		VideoOperationEdit:      {Edit: true},
		VideoOperationExtension: {Extension: true},
	}}
	for _, operation := range []VideoOperation{VideoOperationEdit, VideoOperationExtension} {
		require.NoError(t, catalog.Validate(VideoProviderKling, CanonicalVideoRequest{
			Operation:       operation,
			Model:           "kling-3.0",
			ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}},
		}))
	}
}

func TestValidateVideoCapabilityRequiresDistinctPairedFrameCapability(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderSeedance: {
		VideoOperationGeneration: {FirstFrame: true, LastFrame: true},
	}}
	err := catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{
		Operation:  VideoOperationGeneration,
		Model:      "m",
		FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}},
		LastFrame:  []VideoAsset{{URL: "https://example.com/last.png"}},
	})
	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)
}

func TestValidateVideoCapabilityHonorsDisabledEditAndExtensionFlags(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderKling: {
		VideoOperationEdit:      {Edit: false},
		VideoOperationExtension: {Extension: false},
	}}
	for _, operation := range []VideoOperation{VideoOperationEdit, VideoOperationExtension} {
		require.ErrorIs(t, catalog.Validate(VideoProviderKling, CanonicalVideoRequest{
			Operation:       operation,
			Model:           "kling-3.0",
			ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}},
		}), ErrVideoUnsupportedCapability)
	}
}

func TestValidateVideoCapabilityUsesModelOverridesWithinProvider(t *testing.T) {
	audio := true
	catalog := VideoCapabilityCatalog{
		VideoProviderSeedance: {
			VideoOperationGeneration: {Text: true, Audio: true},
		},
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-audio"): {
			VideoOperationGeneration: {Text: true, Audio: true},
		},
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-silent"): {
			VideoOperationGeneration: {Text: true, Audio: false},
		},
	}

	request := CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model:     "seedance-audio",
		Prompt:    "animate",
		Audio:     &audio,
	}
	require.NoError(t, catalog.Validate(VideoProviderSeedance, request))

	request.Model = "seedance-silent"
	require.ErrorIs(t, catalog.Validate(VideoProviderSeedance, request), ErrVideoUnsupportedCapability)

	request.Model = "seedance-unlisted"
	require.NoError(t, catalog.Validate(VideoProviderSeedance, request), "models without an override keep the provider default")
}
