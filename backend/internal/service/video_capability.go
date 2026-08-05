package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrVideoUnsupportedCapability = infraerrors.BadRequest("VIDEO_UNSUPPORTED_CAPABILITY", "unsupported video capability")

// VideoCapability describes one provider operation. Presence in a provider's
// operation map enables the operation; fields below constrain its input/output
// variants.
type VideoCapability struct {
	Text               bool
	FirstFrame         bool
	LastFrame          bool
	FirstAndLastFrame  bool
	ReferenceImages    bool
	ReferenceVideos    bool
	Edit               bool
	Extension          bool
	Audio              bool
	MinDurationSeconds int
	MaxDurationSeconds int
	Resolutions        []string
	AspectRatios       []string
}

type VideoProviderCapabilities map[VideoOperation]VideoCapability

type VideoCapabilityCatalog map[string]VideoProviderCapabilities

const videoModelCapabilityKeySeparator = "\x00"

// VideoModelCapabilityKey creates an explicit provider/model override key for
// VideoCapabilityCatalog. A plain provider key remains the default for models
// without an override, preserving the original provider-to-operation catalog.
func VideoModelCapabilityKey(provider, model string) string {
	return strings.TrimSpace(provider) + videoModelCapabilityKeySeparator + strings.TrimSpace(model)
}

func (catalog VideoCapabilityCatalog) Validate(provider string, request CanonicalVideoRequest) error {
	provider = strings.TrimSpace(provider)
	providerCapabilities, ok := catalog[VideoModelCapabilityKey(provider, request.Model)]
	if !ok {
		providerCapabilities, ok = catalog[provider]
	}
	if !ok {
		return ErrVideoUnsupportedCapability
	}
	capability, ok := providerCapabilities[request.Operation]
	if !ok {
		return ErrVideoUnsupportedCapability
	}
	if request.Operation == VideoOperationEdit && !capability.Edit {
		return ErrVideoUnsupportedCapability
	}
	if request.Operation == VideoOperationExtension && !capability.Extension {
		return ErrVideoUnsupportedCapability
	}

	if request.Operation == VideoOperationGeneration && request.Prompt != "" && request.assetCount() == 0 && !capability.Text {
		return ErrVideoUnsupportedCapability
	}
	jointFrameRequest := len(request.FirstFrame) > 0 && len(request.LastFrame) > 0
	if jointFrameRequest && !capability.FirstAndLastFrame {
		return ErrVideoUnsupportedCapability
	}
	if len(request.LastFrame) > 0 && !jointFrameRequest && !capability.LastFrame {
		return ErrVideoUnsupportedCapability
	}
	if len(request.FirstFrame) > 0 && !jointFrameRequest && !capability.FirstFrame {
		return ErrVideoUnsupportedCapability
	}
	if len(request.ReferenceImages) > 0 && !capability.ReferenceImages {
		return ErrVideoUnsupportedCapability
	}
	referenceVideoCount := len(request.ReferenceVideos)
	if request.Operation == VideoOperationEdit || request.Operation == VideoOperationExtension {
		referenceVideoCount-- // the first video is the operation's required source
	}
	if referenceVideoCount > 0 && !capability.ReferenceVideos {
		return ErrVideoUnsupportedCapability
	}
	if request.Audio != nil && *request.Audio && !capability.Audio {
		return ErrVideoUnsupportedCapability
	}
	if request.DurationSeconds > 0 {
		if capability.MinDurationSeconds > 0 && request.DurationSeconds < capability.MinDurationSeconds {
			return ErrVideoUnsupportedCapability
		}
		if capability.MaxDurationSeconds > 0 && request.DurationSeconds > capability.MaxDurationSeconds {
			return ErrVideoUnsupportedCapability
		}
	}
	if request.Resolution != "" && len(capability.Resolutions) > 0 && !containsVideoCapabilityValue(capability.Resolutions, request.Resolution) {
		return ErrVideoUnsupportedCapability
	}
	if request.AspectRatio != "" && len(capability.AspectRatios) > 0 && !containsVideoCapabilityValue(capability.AspectRatios, request.AspectRatio) {
		return ErrVideoUnsupportedCapability
	}
	return nil
}

func containsVideoCapabilityValue(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}
