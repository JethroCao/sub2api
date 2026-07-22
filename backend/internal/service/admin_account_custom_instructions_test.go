package service

import (
	"context"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestCreateAccountRejectsOversizeOpenAICustomInstructions(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}
	value := strings.Repeat("界", OpenAICustomInstructionsMaxBytes)

	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "OpenAI",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeOAuth,
		Credentials:          map[string]any{OpenAICustomInstructionsCredentialKey: value},
		SkipDefaultGroupBind: true,
	})

	require.Error(t, err)
	require.Equal(t, "OPENAI_CUSTOM_INSTRUCTIONS_TOO_LONG", infraerrors.Reason(err))
	require.NotContains(t, err.Error(), "界界界")
	require.Empty(t, repo.accounts)
}

func TestCreateAccountAllowsSetupTokenOpenAICustomInstructions(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	account, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "OpenAI setup token",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeSetupToken,
		Credentials:          map[string]any{OpenAICustomInstructionsCredentialKey: "account suffix"},
		SkipDefaultGroupBind: true,
	})

	require.NoError(t, err)
	require.Equal(t, "account suffix", account.GetOpenAICustomInstructions())
}

func TestCreateAccountRejectsUnsupportedOpenAITypeCustomInstructions(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "OpenAI upstream",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeUpstream,
		Credentials:          map[string]any{OpenAICustomInstructionsCredentialKey: "account suffix"},
		SkipDefaultGroupBind: true,
	})

	require.Error(t, err)
	require.Equal(t, "OPENAI_CUSTOM_INSTRUCTIONS_UNSUPPORTED_ACCOUNT_TYPE", infraerrors.Reason(err))
	require.Empty(t, repo.accounts)
}

func TestUpdateAccountRejectsOversizeOpenAICustomInstructionsAfterCredentialMerge(t *testing.T) {
	accountID := int64(1)
	repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:          accountID,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Credentials: map[string]any{"access_token": "token"},
		},
	}}}
	value := strings.Repeat("界", OpenAICustomInstructionsMaxBytes)

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: value},
	})

	require.Error(t, err)
	require.Equal(t, "OPENAI_CUSTOM_INSTRUCTIONS_TOO_LONG", infraerrors.Reason(err))
	require.NotContains(t, err.Error(), "界界界")
	require.NotContains(t, repo.accounts[accountID].Credentials, OpenAICustomInstructionsCredentialKey)
	require.Equal(t, "token", repo.accounts[accountID].Credentials["access_token"])
}

func TestUpdateAccountValidatesOpenAICustomInstructionsAgainstFinalType(t *testing.T) {
	tests := []struct {
		name         string
		storedType   string
		credentials  map[string]any
		input        *UpdateAccountInput
		wantReason   string
		wantType     string
		wantReadback string
	}{
		{
			name:       "rejects change to unsupported type with persisted instructions",
			storedType: AccountTypeOAuth,
			credentials: map[string]any{
				"access_token":                        "token",
				OpenAICustomInstructionsCredentialKey: "account suffix",
			},
			input:      &UpdateAccountInput{Type: AccountTypeUpstream},
			wantReason: "OPENAI_CUSTOM_INSTRUCTIONS_UNSUPPORTED_ACCOUNT_TYPE",
			wantType:   AccountTypeOAuth,
		},
		{
			name:       "allows change to supported type with new instructions",
			storedType: AccountTypeUpstream,
			credentials: map[string]any{
				"api_key": "token",
			},
			input:        &UpdateAccountInput{Type: AccountTypeSetupToken, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "account suffix"}},
			wantType:     AccountTypeSetupToken,
			wantReadback: "account suffix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accountID := int64(1)
			repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				accountID: {ID: accountID, Platform: PlatformOpenAI, Type: tt.storedType, Status: StatusActive, Credentials: tt.credentials},
			}}}

			updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, tt.input)

			if tt.wantReason != "" {
				require.Error(t, err)
				require.Equal(t, tt.wantReason, infraerrors.Reason(err))
				require.Equal(t, tt.wantType, repo.accounts[accountID].Type)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantType, updated.Type)
			require.Equal(t, tt.wantReadback, updated.GetOpenAICustomInstructions())
		})
	}
}
