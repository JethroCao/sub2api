package service

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/collection"
)

func TestProviderSetRegistersVideoPricingService(t *testing.T) {
	require.True(t, providerSetRegisters(t, "NewVideoPricingService"), "service.ProviderSet must provide VideoPricingService to future Wire consumers")
}

func TestProviderSetRegistersVideoBillingService(t *testing.T) {
	require.True(t, providerSetRegisters(t, "NewVideoBillingService"), "service.ProviderSet must provide VideoBillingService to future Wire consumers")
}

func TestProviderSetRegistersDurableVideoRuntime(t *testing.T) {
	for _, provider := range []string{"ProvideDurableVideoProviderRegistry", "ProvideVideoTaskService", "ProvideVideoReconciler", "ProvideVideoRetention", "ProvideVideoRuntime"} {
		require.True(t, providerSetRegisters(t, provider), "service.ProviderSet must register %s", provider)
	}
}

func TestProvideVideoTaskServiceCopiesFailClosedProviderFlags(t *testing.T) {
	cfg := &config.Config{Video: config.VideoConfig{GrokEnabled: true, SeedanceEnabled: false, KlingEnabled: true}}
	service := ProvideVideoTaskService(nil, nil, nil, nil, nil, nil, nil, nil, cfg)
	require.Equal(t, cfg.Video, service.videoConfig)
}

func TestProvideDurableVideoProviderRegistryKeepsKlingGated(t *testing.T) {
	registry, err := ProvideDurableVideoProviderRegistry(nil, nil)
	require.NoError(t, err)
	_, hasGrok := registry.Get(PlatformGrok)
	_, hasSeedance := registry.Get(VideoProviderSeedance)
	_, hasKling := registry.Get(VideoProviderKling)
	require.True(t, hasGrok)
	require.True(t, hasSeedance)
	require.False(t, hasKling, "Kling must remain unregistered until authenticated paid recovery fixtures exist")
}

func TestProductionVideoRegistryCapabilityChainEnablesOnlyDeclaredModels(t *testing.T) {
	registry, err := ProvideDurableVideoProviderRegistry(nil, nil)
	require.NoError(t, err)
	validator := ProvideVideoCapabilityValidator(ProvideVideoCapabilityCatalog(registry))

	require.NoError(t, validator.Validate(PlatformGrok, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "grok-imagine-video", Prompt: "waves",
	}))
	require.NoError(t, validator.Validate(VideoProviderSeedance, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "seedance-2.0", Prompt: "waves",
	}))
	require.ErrorIs(t, validator.Validate(VideoProviderSeedance, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "seedance-unknown", Prompt: "waves",
	}), ErrVideoUnsupportedCapability)
	require.ErrorIs(t, validator.Validate(VideoProviderKling, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "waves",
	}), ErrVideoUnsupportedCapability)
}

func TestProductionVideoRegistryCapabilityChainEnablesOfficialSeedance25GenerationSubset(t *testing.T) {
	registry, err := ProvideDurableVideoProviderRegistry(nil, nil)
	require.NoError(t, err)
	validator := ProvideVideoCapabilityValidator(ProvideVideoCapabilityCatalog(registry))
	generatedAudio := true

	accepted := []CanonicalVideoRequest{
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves"},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", DurationSeconds: 4, Resolution: "480p", AspectRatio: "21:9"},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", DurationSeconds: 30, Resolution: "720p", AspectRatio: "adaptive"},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}}},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}}, LastFrame: []VideoAsset{{URL: "https://example.com/last.png"}}},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", ReferenceImages: []VideoAsset{{URL: "https://example.com/reference.png"}}},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", ReferenceVideos: []VideoAsset{{URL: "https://example.com/reference.mp4"}}},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", Audio: &generatedAudio},
	}
	for _, ratio := range []string{"16:9", "4:3", "1:1", "3:4", "9:16"} {
		accepted = append(accepted, CanonicalVideoRequest{
			Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", AspectRatio: ratio,
		})
	}
	for _, request := range accepted {
		require.NoError(t, validator.Validate(VideoProviderSeedance, request), "request should match the verified Seedance 2.5 generation contract: %#v", request)
	}

	rejected := []CanonicalVideoRequest{
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", LastFrame: []VideoAsset{{URL: "https://example.com/last.png"}}},
		{Operation: VideoOperationEdit, Model: "seedance-2.5", ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}}},
		{Operation: VideoOperationExtension, Model: "seedance-2.5", ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}}},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", DurationSeconds: 3},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", DurationSeconds: 31},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", Resolution: "1080p"},
		{Operation: VideoOperationGeneration, Model: "seedance-2.5", Prompt: "waves", AspectRatio: "2:1"},
		{Operation: VideoOperationGeneration, Model: "seedance-unknown", Prompt: "waves"},
	}
	for _, request := range rejected {
		require.ErrorIs(t, validator.Validate(VideoProviderSeedance, request), ErrVideoUnsupportedCapability, "request must remain fail-closed: %#v", request)
	}
}

func providerSetRegisters(t *testing.T, providerName string) bool {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)

	wireFile, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(testFile), "wire.go"), nil, 0)
	require.NoError(t, err)

	registered := false
	ast.Inspect(wireFile, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewSet" {
			return true
		}
		packageName, ok := selector.X.(*ast.Ident)
		if !ok || packageName.Name != "wire" {
			return true
		}
		for _, argument := range call.Args {
			provider, ok := argument.(*ast.Ident)
			if ok && provider.Name == providerName {
				registered = true
			}
		}
		return false
	})

	return registered
}

func TestProvideTimingWheelService_ReturnsError(t *testing.T) {
	original := newTimingWheel
	t.Cleanup(func() { newTimingWheel = original })

	newTimingWheel = func(_ time.Duration, _ int, _ collection.Execute) (*collection.TimingWheel, error) {
		return nil, errors.New("boom")
	}

	svc, err := ProvideTimingWheelService()
	if err == nil {
		t.Fatalf("期望返回 error，但得到 nil")
	}
	if svc != nil {
		t.Fatalf("期望返回 nil svc，但得到非空")
	}
}

func TestProvideTimingWheelService_Success(t *testing.T) {
	svc, err := ProvideTimingWheelService()
	if err != nil {
		t.Fatalf("期望 err 为 nil，但得到: %v", err)
	}
	if svc == nil {
		t.Fatalf("期望 svc 非空，但得到 nil")
	}
	svc.Stop()
}
