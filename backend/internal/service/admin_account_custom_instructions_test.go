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
