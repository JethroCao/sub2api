//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type authSourceDefaultsRepoStub struct {
	values  map[string]string
	updates map[string]string
}

func (s *authSourceDefaultsRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *authSourceDefaultsRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (s *authSourceDefaultsRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *authSourceDefaultsRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := s.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (s *authSourceDefaultsRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	s.updates = make(map[string]string, len(settings))
	for key, value := range settings {
		s.updates[key] = value
		if s.values == nil {
			s.values = map[string]string{}
		}
		s.values[key] = value
	}
	return nil
}

func (s *authSourceDefaultsRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *authSourceDefaultsRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

func TestSettingService_GetAuthSourceDefaultSettings_ParsesValuesAndDefaults(t *testing.T) {
	repo := &authSourceDefaultsRepoStub{
		values: map[string]string{
			SettingKeyAuthSourceDefaultEmailBalance:            "12.5",
			SettingKeyAuthSourceDefaultEmailConcurrency:        "7",
			SettingKeyAuthSourceDefaultEmailSubscriptions:      `[{"group_id":11,"validity_days":30}]`,
			SettingKeyAuthSourceDefaultEmailGrantOnSignup:      "false",
			SettingKeyAuthSourceDefaultLinuxDoGrantOnFirstBind: "true",
			SettingKeyForceEmailOnThirdPartySignup:             "true",
		},
	}
	svc := NewSettingService(repo, &config.Config{})

	got, err := svc.GetAuthSourceDefaultSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 12.5, got.Email.Balance)
	require.Equal(t, 7, got.Email.Concurrency)
	require.Equal(t, []DefaultSubscriptionSetting{{GroupID: 11, ValidityDays: 30}}, got.Email.Subscriptions)
	require.False(t, got.Email.GrantOnSignup)
	require.False(t, got.Email.GrantOnFirstBind)
	require.Equal(t, 0.0, got.LinuxDo.Balance)
	require.Equal(t, 5, got.LinuxDo.Concurrency)
	require.Equal(t, []DefaultSubscriptionSetting{}, got.LinuxDo.Subscriptions)
	require.False(t, got.LinuxDo.GrantOnSignup)
	require.True(t, got.LinuxDo.GrantOnFirstBind)
	require.Equal(t, 5, got.OIDC.Concurrency)
	require.Equal(t, 5, got.WeChat.Concurrency)
	require.Equal(t, 5, got.Feishu.Concurrency)
	require.False(t, got.OIDC.GrantOnSignup)
	require.False(t, got.WeChat.GrantOnSignup)
	require.False(t, got.Feishu.GrantOnSignup)
	require.True(t, got.ForceEmailOnThirdPartySignup)
}

func TestAuthSourceDefaultSettingsIncludesFeishu(t *testing.T) {
	repo := &authSourceDefaultsRepoStub{
		values: map[string]string{
			SettingKeyAuthSourceDefaultFeishuBalance:          "18.75",
			SettingKeyAuthSourceDefaultFeishuConcurrency:      "9",
			SettingKeyAuthSourceDefaultFeishuSubscriptions:    `[{"group_id":77,"validity_days":45}]`,
			SettingKeyAuthSourceDefaultFeishuGrantOnSignup:    "true",
			SettingKeyAuthSourceDefaultFeishuGrantOnFirstBind: "true",
			SettingKeyAuthSourcePlatformQuotas("feishu"):      `{"openai":{"daily":1.5}}`,
		},
	}
	svc := NewSettingService(repo, &config.Config{})

	got, err := svc.GetAuthSourceDefaultSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 18.75, got.Feishu.Balance)
	require.Equal(t, 9, got.Feishu.Concurrency)
	require.Equal(t, []DefaultSubscriptionSetting{{GroupID: 77, ValidityDays: 45}}, got.Feishu.Subscriptions)
	require.True(t, got.Feishu.GrantOnSignup)
	require.True(t, got.Feishu.GrantOnFirstBind)
	require.NotNil(t, got.Feishu.PlatformQuotas)
	require.NotNil(t, got.Feishu.PlatformQuotas["openai"])
	require.NotNil(t, got.Feishu.PlatformQuotas["openai"].DailyLimitUSD)
	require.Equal(t, 1.5, *got.Feishu.PlatformQuotas["openai"].DailyLimitUSD)
}

func TestSettingService_UpdateAuthSourceDefaultSettings_PersistsAllKeys(t *testing.T) {
	repo := &authSourceDefaultsRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.UpdateAuthSourceDefaultSettings(context.Background(), &AuthSourceDefaultSettings{
		Email: ProviderDefaultGrantSettings{
			Balance:          1.25,
			Concurrency:      3,
			Subscriptions:    []DefaultSubscriptionSetting{{GroupID: 21, ValidityDays: 14}},
			GrantOnSignup:    false,
			GrantOnFirstBind: true,
		},
		LinuxDo: ProviderDefaultGrantSettings{
			Balance:          2,
			Concurrency:      4,
			Subscriptions:    []DefaultSubscriptionSetting{{GroupID: 22, ValidityDays: 30}},
			GrantOnSignup:    true,
			GrantOnFirstBind: false,
		},
		OIDC: ProviderDefaultGrantSettings{
			Balance:          3,
			Concurrency:      5,
			Subscriptions:    []DefaultSubscriptionSetting{{GroupID: 23, ValidityDays: 60}},
			GrantOnSignup:    true,
			GrantOnFirstBind: true,
		},
		WeChat: ProviderDefaultGrantSettings{
			Balance:          4,
			Concurrency:      6,
			Subscriptions:    []DefaultSubscriptionSetting{{GroupID: 24, ValidityDays: 90}},
			GrantOnSignup:    false,
			GrantOnFirstBind: false,
		},
		Feishu: ProviderDefaultGrantSettings{
			Balance:          8.5,
			Concurrency:      10,
			Subscriptions:    []DefaultSubscriptionSetting{{GroupID: 88, ValidityDays: 120}},
			GrantOnSignup:    true,
			GrantOnFirstBind: true,
			PlatformQuotas: map[string]*DefaultPlatformQuotaSetting{
				"openai": {DailyLimitUSD: floatPtrForAuthSourceDefaultsTest(2.25)},
			},
		},
		ForceEmailOnThirdPartySignup: true,
	})
	require.NoError(t, err)
	require.Equal(t, "1.25000000", repo.updates[SettingKeyAuthSourceDefaultEmailBalance])
	require.Equal(t, "3", repo.updates[SettingKeyAuthSourceDefaultEmailConcurrency])
	require.Equal(t, "false", repo.updates[SettingKeyAuthSourceDefaultEmailGrantOnSignup])
	require.Equal(t, "true", repo.updates[SettingKeyAuthSourceDefaultEmailGrantOnFirstBind])
	require.Equal(t, "true", repo.updates[SettingKeyForceEmailOnThirdPartySignup])

	var got []DefaultSubscriptionSetting
	require.NoError(t, json.Unmarshal([]byte(repo.updates[SettingKeyAuthSourceDefaultWeChatSubscriptions]), &got))
	require.Equal(t, []DefaultSubscriptionSetting{{GroupID: 24, ValidityDays: 90}}, got)

	var feishuSubscriptions []DefaultSubscriptionSetting
	require.NoError(t, json.Unmarshal([]byte(repo.updates[SettingKeyAuthSourceDefaultFeishuSubscriptions]), &feishuSubscriptions))
	require.Equal(t, []DefaultSubscriptionSetting{{GroupID: 88, ValidityDays: 120}}, feishuSubscriptions)
	require.Equal(t, "8.50000000", repo.updates[SettingKeyAuthSourceDefaultFeishuBalance])
	require.Equal(t, "10", repo.updates[SettingKeyAuthSourceDefaultFeishuConcurrency])
	require.Equal(t, "true", repo.updates[SettingKeyAuthSourceDefaultFeishuGrantOnSignup])
	require.Equal(t, "true", repo.updates[SettingKeyAuthSourceDefaultFeishuGrantOnFirstBind])
	require.JSONEq(t, `{"openai":{"daily":2.25,"weekly":null,"monthly":null}}`, repo.updates[SettingKeyAuthSourcePlatformQuotas("feishu")])
}

func TestUpdateAuthSourceDefaultSettingsPersistsFeishu(t *testing.T) {
	repo := &authSourceDefaultsRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.UpdateAuthSourceDefaultSettings(context.Background(), &AuthSourceDefaultSettings{
		Feishu: ProviderDefaultGrantSettings{
			Balance:          31.5,
			Concurrency:      11,
			Subscriptions:    []DefaultSubscriptionSetting{{GroupID: 93, ValidityDays: 12}},
			GrantOnSignup:    true,
			GrantOnFirstBind: false,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "31.50000000", repo.updates[SettingKeyAuthSourceDefaultFeishuBalance])
	require.Equal(t, "11", repo.updates[SettingKeyAuthSourceDefaultFeishuConcurrency])
	require.Equal(t, "true", repo.updates[SettingKeyAuthSourceDefaultFeishuGrantOnSignup])
	require.Equal(t, "false", repo.updates[SettingKeyAuthSourceDefaultFeishuGrantOnFirstBind])
	require.JSONEq(t, `[{"group_id":93,"validity_days":12}]`, repo.updates[SettingKeyAuthSourceDefaultFeishuSubscriptions])
}

func TestResolveAuthSourceDefaultsFallsBackWhenFeishuUnset(t *testing.T) {
	repo := &authSourceDefaultsRepoStub{
		values: map[string]string{
			SettingKeyDefaultBalance:       "6.5",
			SettingKeyDefaultConcurrency:   "4",
			SettingKeyDefaultSubscriptions: `[{"group_id":12,"validity_days":7}]`,
		},
	}
	svc := NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserBalance: 6.5, UserConcurrency: 4}})

	got, enabled, err := svc.ResolveAuthSourceGrantSettings(context.Background(), "feishu", false)
	require.NoError(t, err)
	require.False(t, enabled)
	require.Equal(t, 6.5, got.Balance)
	require.Equal(t, 4, got.Concurrency)
	require.Equal(t, []DefaultSubscriptionSetting{{GroupID: 12, ValidityDays: 7}}, got.Subscriptions)
}

func floatPtrForAuthSourceDefaultsTest(v float64) *float64 {
	return &v
}
