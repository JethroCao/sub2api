package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	VideoAccountProbeNotSupportedCode           = "probe_not_supported"
	VideoAccountProbeLocalConfigValidatedStatus = "local_config_validated"
)

var ErrVideoAccountProbeNotSupported = errors.New("video account probe not supported")

const (
	VideoProviderExtraKey             = "video_provider"
	VideoModelMappingExtraKey         = "model_mapping"
	VideoDisabledCapabilitiesExtraKey = "video_disabled_capabilities"
)

var videoAccountCredentialKeys = map[string]struct{}{
	"api_key":    {},
	"access_key": {},
	"secret_key": {},
	"base_url":   {},
}

var videoAccountExtraKeys = map[string]struct{}{
	VideoProviderExtraKey:             {},
	VideoModelMappingExtraKey:         {},
	VideoDisabledCapabilitiesExtraKey: {},
}

var videoCapabilityTags = []string{
	"audio",
	"edit",
	"extension",
	"first_and_last_frame",
	"first_frame",
	"generation",
	"last_frame",
	"reference_images",
	"reference_videos",
	"text",
}

var seedanceAdminCapabilityTags = []string{
	"audio",
	"first_and_last_frame",
	"first_frame",
	"generation",
	"last_frame",
	"reference_images",
	"reference_videos",
	"text",
}

type VideoAccountAdminMetadata struct {
	Provider       string   `json:"video_provider"`
	CapabilityTags []string `json:"video_capabilities"`
}

type VideoAccountProbeResult struct {
	Code   string
	Status string
}

type VideoAccountCredentialProbe interface {
	ProbeCredentials(context.Context, *Account) (VideoAccountProbeResult, error)
}

type LocalVideoAccountProbe struct{}

func NewLocalVideoAccountProbe() *LocalVideoAccountProbe {
	return &LocalVideoAccountProbe{}
}

// ProbeCredentials performs configuration validation only. No verified
// non-generation authentication endpoint exists in the checked-in Seedance
// contract, and Kling remains behind its authenticated paid-contract gate.
func (*LocalVideoAccountProbe) ProbeCredentials(_ context.Context, account *Account) (VideoAccountProbeResult, error) {
	if account == nil {
		return VideoAccountProbeResult{}, infraerrors.BadRequest("VIDEO_ACCOUNT_INVALID", "video account is required")
	}
	if err := validateStoredVideoAccountAdminKeys(account.Credentials, account.Extra); err != nil {
		return VideoAccountProbeResult{}, err
	}
	credentials, extra, err := NormalizeVideoAccountAdminUpdate(account, nil, nil, "")
	if err != nil {
		return VideoAccountProbeResult{}, err
	}
	if err := ValidateVideoAccountAdminFinalConfig(account.Platform, account.Type, account.Status, extra, credentials); err != nil {
		return VideoAccountProbeResult{}, err
	}
	return VideoAccountProbeResult{
		Code:   VideoAccountProbeNotSupportedCode,
		Status: VideoAccountProbeLocalConfigValidatedStatus,
	}, ErrVideoAccountProbeNotSupported
}

func validateStoredVideoAccountAdminKeys(credentials, extra map[string]any) error {
	for key := range credentials {
		if _, ok := videoAccountCredentialKeys[key]; ok || key == VideoModelMappingExtraKey {
			continue
		}
		return infraerrors.BadRequest("VIDEO_CREDENTIAL_KEY_INVALID", fmt.Sprintf("unsupported video credential key: %s", key))
	}
	for key := range extra {
		if _, ok := videoAccountExtraKeys[key]; ok {
			continue
		}
		return infraerrors.BadRequest("VIDEO_EXTRA_KEY_INVALID", fmt.Sprintf("unsupported video extra key: %s", key))
	}
	return nil
}

// NormalizeVideoAccountAdminCreate validates and copies the administrator-owned
// Video account JSON. It deliberately drops no unknown data: unknown keys are
// rejected so callers cannot believe an unapproved provider field was saved.
func NormalizeVideoAccountAdminCreate(platform string, credentials, extra map[string]any) (map[string]any, map[string]any, error) {
	if platform != PlatformVideo {
		return cloneVideoAdminMap(credentials), cloneVideoAdminMap(extra), nil
	}
	normalizedExtra, err := normalizeVideoAccountExtra(nil, extra)
	if err != nil {
		return nil, nil, err
	}
	normalizedCredentials, err := normalizeVideoAccountCredentials(nil, credentials, false)
	if err != nil {
		return nil, nil, err
	}
	if err := validateVideoProviderCredentialShape(normalizedExtra, normalizedCredentials); err != nil {
		return nil, nil, err
	}
	return normalizedCredentials, normalizedExtra, nil
}

// NormalizeVideoAccountAdminUpdate applies a partial request to the stored
// configuration and returns the complete allowlisted final state. Legacy Video
// model mappings stored in credentials are migrated to Extra on the next edit.
func NormalizeVideoAccountAdminUpdate(account *Account, incomingCredentials, incomingExtra map[string]any, requestedStatus string) (map[string]any, map[string]any, error) {
	if account == nil || account.Platform != PlatformVideo {
		return cloneVideoAdminMap(incomingCredentials), cloneVideoAdminMap(incomingExtra), nil
	}

	existingCredentials, err := normalizeStoredVideoAccountCredentials(account.Credentials)
	if err != nil {
		return nil, nil, err
	}
	existingExtra := cloneVideoAdminMap(account.Extra)
	if existingExtra == nil {
		existingExtra = make(map[string]any)
	}
	if _, hasMapping := existingExtra[VideoModelMappingExtraKey]; !hasMapping {
		if legacyMapping, ok := account.Credentials[VideoModelMappingExtraKey]; ok {
			existingExtra[VideoModelMappingExtraKey] = legacyMapping
		}
	}
	normalizedExtra, err := normalizeVideoAccountExtra(existingExtra, incomingExtra)
	if err != nil {
		return nil, nil, err
	}

	explicitDisable := isExplicitVideoAccountDisable(requestedStatus)
	normalizedCredentials, err := normalizeVideoAccountCredentials(existingCredentials, incomingCredentials, explicitDisable)
	if err != nil {
		return nil, nil, err
	}
	if err := validateVideoProviderCredentialShape(normalizedExtra, incomingCredentials); err != nil {
		return nil, nil, err
	}
	pruneVideoCredentialsForProvider(normalizedExtra, normalizedCredentials)
	if err := validateVideoProviderCredentialShape(normalizedExtra, normalizedCredentials); err != nil {
		return nil, nil, err
	}
	return normalizedCredentials, normalizedExtra, nil
}

// ValidateVideoAccountAdminFinalConfig validates the final merged state while
// permitting an explicitly disabled account to have its secrets removed.
func ValidateVideoAccountAdminFinalConfig(platform, accountType, status string, extra, credentials map[string]any) error {
	if platform != PlatformVideo {
		return nil
	}
	if accountType != AccountTypeAPIKey {
		return infraerrors.BadRequest("VIDEO_ACCOUNT_TYPE_INVALID", "video accounts require API key account type")
	}
	provider, _ := extra[VideoProviderExtraKey].(string)
	if provider != VideoProviderSeedance && provider != VideoProviderKling {
		return infraerrors.BadRequest("VIDEO_PROVIDER_INVALID", "video provider must be seedance or kling")
	}
	if isVideoAccountDisabledStatus(status) {
		return nil
	}
	return ValidateVideoAccountConfig(platform, accountType, extra, credentials)
}

// BuildVideoAccountAdminMetadata derives display-only provider capabilities
// without copying credentials or provider secrets into the response object.
func BuildVideoAccountAdminMetadata(account *Account) VideoAccountAdminMetadata {
	if account == nil || account.Platform != PlatformVideo {
		return VideoAccountAdminMetadata{}
	}
	provider := account.VideoProvider()
	var supported []string
	if provider == VideoProviderSeedance {
		supported = seedanceAdminCapabilityTags
	}
	disabled := videoDisabledCapabilitySet(account.Extra[VideoDisabledCapabilitiesExtraKey])
	tags := make([]string, 0, len(supported))
	for _, tag := range supported {
		if _, blocked := disabled[tag]; !blocked {
			tags = append(tags, tag)
		}
	}
	return VideoAccountAdminMetadata{Provider: provider, CapabilityTags: tags}
}

// ValidateVideoAccountCapabilityOverrides applies administrator-disabled
// capabilities after the provider catalog has accepted a request.
func ValidateVideoAccountCapabilityOverrides(account *Account, request CanonicalVideoRequest) error {
	if account == nil || account.Platform != PlatformVideo {
		return nil
	}
	disabled := videoDisabledCapabilitySet(account.Extra[VideoDisabledCapabilitiesExtraKey])
	requires := func(tag string) error {
		if _, blocked := disabled[tag]; blocked {
			return ErrVideoUnsupportedCapability
		}
		return nil
	}

	switch request.Operation {
	case VideoOperationGeneration:
		if err := requires("generation"); err != nil {
			return err
		}
		if request.Prompt != "" && request.assetCount() == 0 {
			if err := requires("text"); err != nil {
				return err
			}
		}
	case VideoOperationEdit:
		if err := requires("edit"); err != nil {
			return err
		}
	case VideoOperationExtension:
		if err := requires("extension"); err != nil {
			return err
		}
	}

	jointFrameRequest := len(request.FirstFrame) > 0 && len(request.LastFrame) > 0
	if jointFrameRequest {
		if err := requires("first_and_last_frame"); err != nil {
			return err
		}
	} else if len(request.FirstFrame) > 0 {
		if err := requires("first_frame"); err != nil {
			return err
		}
	} else if len(request.LastFrame) > 0 {
		if err := requires("last_frame"); err != nil {
			return err
		}
	}
	if len(request.ReferenceImages) > 0 {
		if err := requires("reference_images"); err != nil {
			return err
		}
	}
	referenceVideoCount := len(request.ReferenceVideos)
	if request.Operation == VideoOperationEdit || request.Operation == VideoOperationExtension {
		referenceVideoCount--
	}
	if referenceVideoCount > 0 {
		if err := requires("reference_videos"); err != nil {
			return err
		}
	}
	if request.Audio != nil && *request.Audio {
		return requires("audio")
	}
	return nil
}

func normalizeVideoAccountCredentials(existing, incoming map[string]any, allowSecretClear bool) (map[string]any, error) {
	result := cloneVideoAdminMap(existing)
	if result == nil {
		result = make(map[string]any)
	}
	if incoming == nil {
		return normalizeVideoBaseURL(result)
	}
	for key, value := range incoming {
		if _, ok := videoAccountCredentialKeys[key]; !ok {
			return nil, infraerrors.BadRequest("VIDEO_CREDENTIAL_KEY_INVALID", fmt.Sprintf("unsupported video credential key: %s", key))
		}
		if key == "base_url" {
			if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
				delete(result, key)
				continue
			}
			if _, ok := value.(string); !ok {
				return nil, infraerrors.BadRequest("VIDEO_BASE_URL_INVALID", "video credentials.base_url must be an HTTPS URL")
			}
			result[key] = value
			continue
		}
		secret, ok := value.(string)
		if !ok || strings.TrimSpace(secret) == "" {
			if !allowSecretClear {
				return nil, infraerrors.BadRequest("VIDEO_SECRET_CLEAR_REQUIRES_DISABLE", "video account secrets may only be cleared while explicitly disabling the account")
			}
			delete(result, key)
			continue
		}
		result[key] = secret
	}
	return normalizeVideoBaseURL(result)
}

func normalizeStoredVideoAccountCredentials(stored map[string]any) (map[string]any, error) {
	result := make(map[string]any)
	for key, value := range stored {
		if _, ok := videoAccountCredentialKeys[key]; !ok {
			continue
		}
		result[key] = value
	}
	return normalizeVideoBaseURL(result)
}

func normalizeVideoBaseURL(credentials map[string]any) (map[string]any, error) {
	raw, exists := credentials["base_url"]
	if !exists || raw == nil || strings.TrimSpace(fmt.Sprint(raw)) == "" {
		delete(credentials, "base_url")
		return credentials, nil
	}
	value, ok := raw.(string)
	if !ok {
		return nil, infraerrors.BadRequest("VIDEO_BASE_URL_INVALID", "video credentials.base_url must be an HTTPS URL")
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(value, urlvalidator.ValidationOptions{})
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "VIDEO_BASE_URL_INVALID", "video credentials.base_url must be a valid public HTTPS URL")
	}
	credentials["base_url"] = normalized
	return credentials, nil
}

func normalizeVideoAccountExtra(existing, incoming map[string]any) (map[string]any, error) {
	result := make(map[string]any)
	for key, value := range existing {
		if _, ok := videoAccountExtraKeys[key]; ok {
			result[key] = value
		}
	}
	for key, value := range incoming {
		if _, ok := videoAccountExtraKeys[key]; !ok {
			return nil, infraerrors.BadRequest("VIDEO_EXTRA_KEY_INVALID", fmt.Sprintf("unsupported video extra key: %s", key))
		}
		if value == nil {
			delete(result, key)
			continue
		}
		result[key] = value
	}

	provider, ok := result[VideoProviderExtraKey].(string)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !ok || (provider != VideoProviderSeedance && provider != VideoProviderKling) {
		return nil, infraerrors.BadRequest("VIDEO_PROVIDER_INVALID", "video provider must be seedance or kling")
	}
	result[VideoProviderExtraKey] = provider

	if rawMapping, exists := result[VideoModelMappingExtraKey]; exists {
		mapping, err := normalizeVideoModelMapping(rawMapping)
		if err != nil {
			return nil, err
		}
		if len(mapping) == 0 {
			delete(result, VideoModelMappingExtraKey)
		} else {
			result[VideoModelMappingExtraKey] = mapping
		}
	}
	if rawDisabled, exists := result[VideoDisabledCapabilitiesExtraKey]; exists {
		disabled, err := normalizeVideoDisabledCapabilities(rawDisabled)
		if err != nil {
			return nil, err
		}
		result[VideoDisabledCapabilitiesExtraKey] = disabled
	}
	return result, nil
}

func normalizeVideoModelMapping(raw any) (map[string]any, error) {
	result := make(map[string]any)
	switch mapping := raw.(type) {
	case map[string]any:
		for external, value := range mapping {
			upstream, ok := value.(string)
			if !ok || strings.TrimSpace(external) == "" || strings.TrimSpace(upstream) == "" {
				return nil, infraerrors.BadRequest("VIDEO_MODEL_MAPPING_INVALID", "video extra.model_mapping must map non-empty model names to non-empty strings")
			}
			result[strings.TrimSpace(external)] = strings.TrimSpace(upstream)
		}
	case map[string]string:
		for external, upstream := range mapping {
			if strings.TrimSpace(external) == "" || strings.TrimSpace(upstream) == "" {
				return nil, infraerrors.BadRequest("VIDEO_MODEL_MAPPING_INVALID", "video extra.model_mapping must map non-empty model names to non-empty strings")
			}
			result[strings.TrimSpace(external)] = strings.TrimSpace(upstream)
		}
	default:
		return nil, infraerrors.BadRequest("VIDEO_MODEL_MAPPING_INVALID", "video extra.model_mapping must be an object")
	}
	return result, nil
}

func normalizeVideoDisabledCapabilities(raw any) ([]string, error) {
	values := make([]string, 0)
	switch typed := raw.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			value, ok := item.(string)
			if !ok {
				return nil, infraerrors.BadRequest("VIDEO_CAPABILITY_INVALID", "video disabled capabilities must be strings")
			}
			values = append(values, value)
		}
	default:
		return nil, infraerrors.BadRequest("VIDEO_CAPABILITY_INVALID", "video disabled capabilities must be an array")
	}
	known := make(map[string]struct{}, len(videoCapabilityTags))
	for _, tag := range videoCapabilityTags {
		known[tag] = struct{}{}
	}
	unique := make(map[string]struct{}, len(values))
	for _, rawValue := range values {
		value := strings.ToLower(strings.TrimSpace(rawValue))
		if _, ok := known[value]; !ok {
			return nil, infraerrors.BadRequest("VIDEO_CAPABILITY_INVALID", fmt.Sprintf("unknown video capability: %s", rawValue))
		}
		unique[value] = struct{}{}
	}
	normalized := make([]string, 0, len(unique))
	for value := range unique {
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func validateVideoProviderCredentialShape(extra, credentials map[string]any) error {
	provider, _ := extra[VideoProviderExtraKey].(string)
	switch provider {
	case VideoProviderSeedance:
		if _, exists := credentials["access_key"]; exists {
			return infraerrors.BadRequest("VIDEO_CREDENTIAL_KEY_INVALID", "seedance accounts do not accept credentials.access_key")
		}
		if _, exists := credentials["secret_key"]; exists {
			return infraerrors.BadRequest("VIDEO_CREDENTIAL_KEY_INVALID", "seedance accounts do not accept credentials.secret_key")
		}
	case VideoProviderKling:
		if _, exists := credentials["api_key"]; exists {
			return infraerrors.BadRequest("VIDEO_CREDENTIAL_KEY_INVALID", "kling accounts do not accept credentials.api_key")
		}
	}
	return nil
}

func pruneVideoCredentialsForProvider(extra, credentials map[string]any) {
	provider, _ := extra[VideoProviderExtraKey].(string)
	switch provider {
	case VideoProviderSeedance:
		delete(credentials, "access_key")
		delete(credentials, "secret_key")
	case VideoProviderKling:
		delete(credentials, "api_key")
	}
}

func videoDisabledCapabilitySet(raw any) map[string]struct{} {
	values, err := normalizeVideoDisabledCapabilities(raw)
	if err != nil {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func isExplicitVideoAccountDisable(status string) bool {
	return status == "inactive" || status == StatusDisabled
}

func isVideoAccountDisabledStatus(status string) bool {
	return status == "inactive" || status == StatusDisabled
}

func normalizeVideoAccountAdminStatus(status string) string {
	if status == "inactive" {
		return StatusDisabled
	}
	return status
}

func cloneVideoAdminMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
