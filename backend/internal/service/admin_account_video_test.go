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
		Credentials: map[string]any{"region": "cn"},
	})

	require.NoError(t, err)
	require.Equal(t, "ark-key", updated.Credentials["api_key"])
	require.Equal(t, "cn", updated.Credentials["region"])
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
		Credentials: map[string]any{"region": "cn"},
	})

	require.NoError(t, err)
	require.Equal(t, "ak", updated.Credentials["access_key"])
	require.Equal(t, "sk", updated.Credentials["secret_key"])
}
