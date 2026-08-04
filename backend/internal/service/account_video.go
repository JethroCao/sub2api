package service

import (
	"fmt"
	"strings"
)

const videoProviderExtraKey = "video_provider"

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
		return fmt.Errorf("video accounts require account type %q", AccountTypeAPIKey)
	}
	provider, _ := extra[videoProviderExtraKey].(string)
	switch provider {
	case VideoProviderSeedance:
		if !hasVideoCredential(credentials, "api_key") {
			return fmt.Errorf("video provider %q requires credentials.api_key", VideoProviderSeedance)
		}
	case VideoProviderKling:
		if !hasVideoCredential(credentials, "access_key") || !hasVideoCredential(credentials, "secret_key") {
			return fmt.Errorf("video provider %q requires credentials.access_key and credentials.secret_key", VideoProviderKling)
		}
	default:
		return fmt.Errorf("video provider must be %q or %q", VideoProviderSeedance, VideoProviderKling)
	}
	return nil
}

func hasVideoCredential(credentials map[string]any, key string) bool {
	value, ok := credentials[key].(string)
	return ok && strings.TrimSpace(value) != ""
}
