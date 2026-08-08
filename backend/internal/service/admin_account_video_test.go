package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateAccountRejectsInvalidVideoConfig(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "invalid video",
		Platform:             PlatformVideo,
		Type:                 AccountTypeOAuth,
		Extra:                map[string]any{"video_provider": VideoProviderSeedance},
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
			Extra:       map[string]any{"video_provider": VideoProviderSeedance},
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
			Extra:       map[string]any{"video_provider": VideoProviderSeedance},
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
			Extra:    map[string]any{"video_provider": VideoProviderKling},
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
		Name:                 "disabled seedance",
		Platform:             PlatformVideo,
		Type:                 AccountTypeAPIKey,
		Status:               "inactive",
		Credentials:          map[string]any{"api_key": "ark-key"},
		Extra:                map[string]any{"video_provider": VideoProviderSeedance},
		SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, created.Status)

	_, err = (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "invalid disabled seedance",
		Platform:             PlatformVideo,
		Type:                 AccountTypeAPIKey,
		Status:               "inactive",
		Credentials:          map[string]any{},
		Extra:                map[string]any{"video_provider": VideoProviderSeedance},
		SkipDefaultGroupBind: true,
	})
	require.Error(t, err)
}

func TestUpdateAccountValidatesFinalVideoProviderAndCredentials(t *testing.T) {
	const accountID int64 = 4
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformVideo,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Extra:       map[string]any{"video_provider": VideoProviderSeedance},
			Credentials: map[string]any{"api_key": "ark-key"},
		},
	}}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"video_provider": VideoProviderKling},
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
			Extra:       map[string]any{"video_provider": VideoProviderSeedance},
			Credentials: map[string]any{"api_key": "old-ark-key"},
		},
	}}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{"access_key": "new-access", "secret_key": "new-secret"},
		Extra:       map[string]any{"video_provider": VideoProviderKling},
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
				Extra:    map[string]any{"video_provider": VideoProviderKling},
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
