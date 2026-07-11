package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type feishuSettingsRepoStub struct {
	values  map[string]string
	updates map[string]string
}

func (s *feishuSettingsRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *feishuSettingsRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	if s.values != nil {
		if value, ok := s.values[key]; ok {
			return value, nil
		}
	}
	return "", ErrSettingNotFound
}

func (s *feishuSettingsRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *feishuSettingsRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if s.values != nil {
			if value, ok := s.values[key]; ok {
				out[key] = value
			}
		}
	}
	return out, nil
}

func (s *feishuSettingsRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	s.updates = make(map[string]string, len(settings))
	if s.values == nil {
		s.values = map[string]string{}
	}
	for key, value := range settings {
		s.updates[key] = value
		s.values[key] = value
	}
	return nil
}

func (s *feishuSettingsRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.values))
	for key, value := range s.values {
		out[key] = value
	}
	return out, nil
}

func (s *feishuSettingsRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

func TestGetFeishuConnectSettingsDefaults(t *testing.T) {
	svc := NewSettingService(&feishuSettingsRepoStub{}, &config.Config{})

	got, err := svc.GetAllSettings(context.Background())
	require.NoError(t, err)

	require.False(t, got.FeishuConnectEnabled)
	require.False(t, got.FeishuConnectAppSecretConfigured)
	require.Equal(t, "internal_only", got.FeishuConnectTenantRestrictionPolicy)
	require.False(t, got.FeishuConnectBypassRegistration)
	require.False(t, got.FeishuOrgSyncEnabled)
	require.Equal(t, "auto_disable", got.FeishuDepartedUserAction)
	require.Equal(t, 10, got.FeishuSyncDisableThresholdCount)
	require.Equal(t, 20, got.FeishuSyncDisableThresholdPercent)
	require.True(t, got.EmailPasswordLoginEnabled)
	require.True(t, got.AdminEmailLoginFallbackEnabled)
}

func TestUpdateSecuritySettingsRejectsNoLoginEntry(t *testing.T) {
	svc := NewSettingService(&feishuSettingsRepoStub{}, &config.Config{})

	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		LoginEntrySettingsExplicit:     true,
		EmailPasswordLoginEnabled:      false,
		AdminEmailLoginFallbackEnabled: true,
		FeishuConnectEnabled:           false,
		LinuxDoConnectEnabled:          false,
		DingTalkConnectEnabled:         false,
		WeChatConnectEnabled:           false,
		OIDCConnectEnabled:             false,
		GitHubOAuthEnabled:             false,
		GoogleOAuthEnabled:             false,
	})

	require.Error(t, err)
	require.Equal(t, "LOGIN_ENTRY_UNAVAILABLE", infraerrors.Reason(err))
}

func TestUpdateSecuritySettingsRejectsFeishuOnlyWithoutHealthyConfig(t *testing.T) {
	svc := NewSettingService(&feishuSettingsRepoStub{}, &config.Config{})

	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		LoginEntrySettingsExplicit:     true,
		EmailPasswordLoginEnabled:      false,
		AdminEmailLoginFallbackEnabled: true,
		FeishuConnectEnabled:           true,
	})

	require.Error(t, err)
	require.Equal(t, "FEISHU_CONFIG_INCOMPLETE", infraerrors.Reason(err))
}

func TestUpdateSecuritySettingsAllowsAdminEmailFallback(t *testing.T) {
	repo := &feishuSettingsRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		LoginEntrySettingsExplicit:           true,
		EmailPasswordLoginEnabled:            false,
		AdminEmailLoginFallbackEnabled:       true,
		FeishuConnectEnabled:                 true,
		FeishuConnectAppID:                   "cli_test",
		FeishuConnectAppSecret:               "secret",
		FeishuConnectRedirectURL:             "https://example.com/oauth/feishu/callback",
		FeishuConnectTenantRestrictionPolicy: "internal_only",
		FeishuDepartedUserAction:             "auto_disable",
		FeishuSyncDisableThresholdCount:      10,
		FeishuSyncDisableThresholdPercent:    20,
	})

	require.NoError(t, err)
	require.Equal(t, "false", repo.updates[SettingKeyEmailPasswordLoginEnabled])
	require.Equal(t, "true", repo.updates[SettingKeyAdminEmailLoginFallbackEnabled])
	require.Equal(t, "true", repo.updates[SettingKeyFeishuConnectEnabled])
}
