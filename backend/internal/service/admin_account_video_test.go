package service

import (
	"context"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestCreateAccountRejectsInvalidVideoConfig(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:     "invalid video",
		Platform: PlatformVideo,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
			"model_mapping":  map[string]any{"seedance-2.0": "endpoint-id"},
		},
		Credentials:          map[string]any{"api_key": "ark-key"},
		SkipDefaultGroupBind: true,
	})

	require.Error(t, err)
	require.Empty(t, repo.accounts)
}

func TestUpdateAccountValidatesVideoConfigAfterCredentialMerge(t *testing.T) {
	const accountID int64 = 1
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformVideo,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			Credentials: map[string]any{"api_key": "ark-key"},
		},
	}}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{"base_url": "https://ark.example.com/"},
	})

	require.NoError(t, err)
	require.Equal(t, "ark-key", updated.Credentials["api_key"])
	require.Equal(t, "https://ark.example.com", updated.Credentials["base_url"])
}

func TestUpdateAccountRejectsVideoTypeChange(t *testing.T) {
	const accountID int64 = 2
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformVideo,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			Credentials: map[string]any{"api_key": "ark-key"},
		},
	}}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Type: AccountTypeOAuth,
	})

	require.Error(t, err)
	require.Equal(t, AccountTypeAPIKey, repo.accounts[accountID].Type)
}

func TestUpdateAccountPreservesMaskedKlingCredentials(t *testing.T) {
	const accountID int64 = 3
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformVideo,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra:    map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
			Credentials: map[string]any{
				"access_key": "ak",
				"secret_key": "sk",
			},
		},
	}}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{"base_url": "https://kling.example.com/"},
	})

	require.NoError(t, err)
	require.Equal(t, "ak", updated.Credentials["access_key"])
	require.Equal(t, "sk", updated.Credentials["secret_key"])
	require.Equal(t, "https://kling.example.com", updated.Credentials["base_url"])
}

func TestCreateAccountStoresOnlyNormalizedVideoAdminConfig(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "seedance",
		Platform:             PlatformVideo,
		Type:                 AccountTypeAPIKey,
		SkipDefaultGroupBind: true,
		Credentials: map[string]any{
			"api_key":  "ark-key",
			"base_url": "https://ark.example.com/",
		},
		Extra: map[string]any{
			"video_provider":              VideoProviderSeedance,
			"model_mapping":               map[string]any{"seedance-2.0": "ep-seedance"},
			"video_disabled_capabilities": []any{"audio"},
		},
	})

	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"api_key":  "ark-key",
		"base_url": "https://ark.example.com",
	}, created.Credentials)
	require.Equal(t, map[string]any{
		"video_provider":              VideoProviderSeedance,
		"model_mapping":               map[string]any{"seedance-2.0": "ep-seedance"},
		"video_disabled_capabilities": []string{"audio"},
	}, created.Extra)
}

func TestCreateAccountPersistsExplicitVideoStatusButStillRequiresCredentials(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:        "disabled seedance",
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Status:      "inactive",
		Credentials: map[string]any{"api_key": "ark-key"},
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
			"model_mapping":  map[string]any{"seedance-2.0": "endpoint-id"},
		},
		SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, created.Status)

	_, err = (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:        "invalid disabled seedance",
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Status:      "inactive",
		Credentials: map[string]any{},
		Extra: map[string]any{
			"video_provider": VideoProviderSeedance,
			"model_mapping":  map[string]any{"seedance-2.0": "endpoint-id"},
		},
		SkipDefaultGroupBind: true,
	})
	require.Error(t, err)
}

func TestCreateAccountVideoStatusFieldDoesNotChangeNonVideoDefaultStatus(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "anthropic remains active",
		Platform:             PlatformAnthropic,
		Type:                 AccountTypeAPIKey,
		Status:               StatusDisabled,
		Credentials:          map[string]any{"api_key": "anthropic-key"},
		SkipDefaultGroupBind: true,
	})

	require.NoError(t, err)
	require.Equal(t, StatusActive, created.Status)
}

func TestUpdateAccountValidatesFinalVideoProviderAndCredentials(t *testing.T) {
	const accountID int64 = 4
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformVideo,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			Credentials: map[string]any{"api_key": "ark-key"},
		},
	}}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
	})

	require.Error(t, err)
	require.Equal(t, VideoProviderSeedance, repo.accounts[accountID].Extra["video_provider"])
	require.Equal(t, "ark-key", repo.accounts[accountID].Credentials["api_key"])
}

func TestUpdateAccountSwitchesVideoProviderUsingOnlyNewProviderCredentials(t *testing.T) {
	const accountID int64 = 6
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformVideo,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			Credentials: map[string]any{"api_key": "old-ark-key"},
		},
	}}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{"access_key": "new-access", "secret_key": "new-secret"},
		Extra:       map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
	})

	require.NoError(t, err)
	require.Equal(t, VideoProviderKling, updated.Extra["video_provider"])
	require.NotContains(t, updated.Credentials, "api_key")
	require.Equal(t, "new-access", updated.Credentials["access_key"])
	require.Equal(t, "new-secret", updated.Credentials["secret_key"])
}

func TestUpdateAccountSecretClearPersistsOnlyWithExplicitInactive(t *testing.T) {
	const accountID int64 = 5
	newRepo := func() *upstreamBillingProbeAdminRepo {
		return &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			accountID: {
				ID:       accountID,
				Platform: PlatformVideo,
				Type:     AccountTypeAPIKey,
				Status:   StatusActive,
				Extra:    map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
				Credentials: map[string]any{
					"access_key": "access",
					"secret_key": "secret",
				},
			},
		}}}
	}

	activeRepo := newRepo()
	_, err := (&adminServiceImpl{accountRepo: activeRepo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{"access_key": nil, "secret_key": ""},
	})
	require.Error(t, err)
	require.Equal(t, "secret", activeRepo.accounts[accountID].Credentials["secret_key"])

	inactiveRepo := newRepo()
	updated, err := (&adminServiceImpl{accountRepo: inactiveRepo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Status:      "inactive",
		Credentials: map[string]any{"access_key": nil, "secret_key": ""},
	})
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, updated.Status)
	require.NotContains(t, updated.Credentials, "access_key")
	require.NotContains(t, updated.Credentials, "secret_key")
}

func TestUpdateAccountExplicitDisableClearsLegacyInvalidMappingAndSecrets(t *testing.T) {
	const accountID int64 = 7
	for _, tt := range []struct {
		name        string
		credentials map[string]any
		extra       map[string]any
	}{
		{
			name:        "missing legacy mapping explicitly cleared",
			credentials: map[string]any{"api_key": "ark-key"},
			extra:       map[string]any{VideoModelMappingExtraKey: map[string]any{}},
		},
		{name: "wildcard legacy mapping", credentials: map[string]any{
			"api_key":       "ark-key",
			"model_mapping": map[string]any{"seedance-*": "endpoint-id"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				accountID: {
					ID:          accountID,
					Platform:    PlatformVideo,
					Type:        AccountTypeAPIKey,
					Status:      StatusActive,
					Schedulable: true,
					Extra:       map[string]any{VideoProviderExtraKey: VideoProviderSeedance},
					Credentials: tt.credentials,
				},
			}}}

			updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
				Status:      "inactive",
				Credentials: map[string]any{"api_key": nil},
				Extra:       tt.extra,
			})

			require.NoError(t, err)
			require.Equal(t, StatusDisabled, updated.Status)
			require.Equal(t, map[string]any{}, updated.Credentials)
			require.Equal(t, map[string]any{VideoProviderExtraKey: VideoProviderSeedance}, updated.Extra)
			require.False(t, updated.IsSchedulable())
			require.Equal(t, StatusDisabled, repo.accounts[accountID].Status)
			require.Equal(t, map[string]any{}, repo.accounts[accountID].Credentials)
			require.NotContains(t, repo.accounts[accountID].Extra, VideoModelMappingExtraKey)
		})
	}
}

func TestUpdateAccountMappingExemptionRequiresExplicitDisable(t *testing.T) {
	const accountID int64 = 8
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformVideo,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Extra:       map[string]any{VideoProviderExtraKey: VideoProviderSeedance},
			Credentials: map[string]any{"api_key": "ark-key"},
		},
	}}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Status: StatusActive,
		Extra:  map[string]any{VideoModelMappingExtraKey: map[string]any{}},
	})

	require.Equal(t, "VIDEO_MODEL_MAPPING_REQUIRED", infraerrors.Reason(err))
	require.Equal(t, StatusActive, repo.accounts[accountID].Status)
	require.Equal(t, "ark-key", repo.accounts[accountID].Credentials["api_key"])
}

func TestUpdateAccountReactivationRequiresCredentialsAndExactMapping(t *testing.T) {
	const accountID int64 = 11
	for _, tt := range []struct {
		name        string
		credentials map[string]any
		extra       map[string]any
		wantReason  string
	}{
		{
			name:        "missing mapping",
			credentials: map[string]any{"api_key": "new-key"},
			wantReason:  "VIDEO_MODEL_MAPPING_REQUIRED",
		},
		{
			name:       "missing credentials",
			extra:      map[string]any{VideoModelMappingExtraKey: map[string]any{"seedance-2.0": "endpoint-id"}},
			wantReason: "VIDEO_SEEDANCE_API_KEY_REQUIRED",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				accountID: {
					ID:          accountID,
					Platform:    PlatformVideo,
					Type:        AccountTypeAPIKey,
					Status:      StatusDisabled,
					Schedulable: true,
					Extra:       map[string]any{VideoProviderExtraKey: VideoProviderSeedance},
					Credentials: map[string]any{},
				},
			}}}

			_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
				Status:      StatusActive,
				Credentials: tt.credentials,
				Extra:       tt.extra,
			})

			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
			require.Equal(t, StatusDisabled, repo.accounts[accountID].Status)
			require.Empty(t, repo.accounts[accountID].Credentials)
			require.NotContains(t, repo.accounts[accountID].Extra, VideoModelMappingExtraKey)
		})
	}
}

func TestUpdateAccountExplicitDisableRejectsNewInvalidMapping(t *testing.T) {
	const accountID int64 = 9
	for _, mapping := range []any{
		map[string]any{"seedance-*": "endpoint-id"},
		map[string]any{"seedance-2.0": 42},
	} {
		repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			accountID: {
				ID:          accountID,
				Platform:    PlatformVideo,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Extra:       map[string]any{VideoProviderExtraKey: VideoProviderSeedance},
				Credentials: map[string]any{"api_key": "ark-key"},
			},
		}}}

		_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
			Status:      StatusDisabled,
			Credentials: map[string]any{"api_key": nil},
			Extra:       map[string]any{VideoModelMappingExtraKey: mapping},
		})

		require.Equal(t, "VIDEO_MODEL_MAPPING_INVALID", infraerrors.Reason(err))
		require.Equal(t, StatusActive, repo.accounts[accountID].Status)
		require.Equal(t, "ark-key", repo.accounts[accountID].Credentials["api_key"])
	}
}

func TestUpdateAccountExplicitDisableStillValidatesNonMappingConfiguration(t *testing.T) {
	const accountID int64 = 10
	tests := []struct {
		name        string
		credentials map[string]any
		extra       map[string]any
		wantReason  string
	}{
		{name: "provider", extra: map[string]any{VideoProviderExtraKey: "unknown"}, wantReason: "VIDEO_PROVIDER_INVALID"},
		{name: "credential allowlist", credentials: map[string]any{"api_key": nil, "region": "cn"}, extra: map[string]any{VideoModelMappingExtraKey: map[string]any{}}, wantReason: "VIDEO_CREDENTIAL_KEY_INVALID"},
		{name: "extra allowlist", credentials: map[string]any{"api_key": nil}, extra: map[string]any{VideoModelMappingExtraKey: map[string]any{}, "provider_state": true}, wantReason: "VIDEO_EXTRA_KEY_INVALID"},
		{name: "base url", credentials: map[string]any{"api_key": nil, "base_url": "https://ark.example.com/unverified"}, extra: map[string]any{VideoModelMappingExtraKey: map[string]any{}}, wantReason: "VIDEO_BASE_URL_INVALID"},
		{name: "disabled capability", credentials: map[string]any{"api_key": nil}, extra: map[string]any{VideoModelMappingExtraKey: map[string]any{}, VideoDisabledCapabilitiesExtraKey: []any{"telepathy"}}, wantReason: "VIDEO_CAPABILITY_INVALID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				accountID: {
					ID:          accountID,
					Platform:    PlatformVideo,
					Type:        AccountTypeAPIKey,
					Status:      StatusActive,
					Schedulable: true,
					Extra:       map[string]any{VideoProviderExtraKey: VideoProviderSeedance},
					Credentials: map[string]any{"api_key": "ark-key"},
				},
			}}}

			_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
				Status:      StatusDisabled,
				Credentials: tt.credentials,
				Extra:       tt.extra,
			})

			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
			require.Equal(t, StatusActive, repo.accounts[accountID].Status)
			require.Equal(t, "ark-key", repo.accounts[accountID].Credentials["api_key"])
		})
	}
}
