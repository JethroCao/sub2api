package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const videoProviderExtraKey = VideoProviderExtraKey

// VideoProvider returns the configured provider for a Video account.
func (a *Account) VideoProvider() string {
	if a == nil || a.Platform != PlatformVideo {
		return ""
	}
	provider, _ := a.Extra[videoProviderExtraKey].(string)
	return strings.ToLower(strings.TrimSpace(provider))
}

// ValidateVideoAccountConfig verifies provider-specific credentials for Video accounts.
func ValidateVideoAccountConfig(platform, accountType string, extra, credentials map[string]any) error {
	if platform != PlatformVideo {
		return nil
	}
	if accountType != AccountTypeAPIKey {
		return infraerrors.BadRequest("VIDEO_ACCOUNT_TYPE_INVALID", "video accounts require API key account type")
	}
	provider, _ := extra[videoProviderExtraKey].(string)
	switch provider {
	case VideoProviderSeedance:
		if !hasVideoCredential(credentials, "api_key") {
			return infraerrors.BadRequest("VIDEO_SEEDANCE_API_KEY_REQUIRED", "seedance video accounts require credentials.api_key")
		}
	case VideoProviderKling:
		if !hasVideoCredential(credentials, "access_key") || !hasVideoCredential(credentials, "secret_key") {
			return infraerrors.BadRequest("VIDEO_KLING_CREDENTIALS_REQUIRED", "kling video accounts require credentials.access_key and credentials.secret_key")
		}
	default:
		return infraerrors.BadRequest("VIDEO_PROVIDER_INVALID", "video provider must be seedance or kling")
	}
	return nil
}

func hasVideoCredential(credentials map[string]any, key string) bool {
	value, ok := credentials[key].(string)
	return ok && strings.TrimSpace(value) != ""
}
