package service

import (
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAccountGetOpenAICustomInstructions(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    string
	}{
		{name: "nil account", want: ""},
		{name: "non OpenAI account", account: &Account{Platform: PlatformGemini, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "instructions"}}, want: ""},
		{name: "wrong credential type", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: true}}, want: ""},
		{name: "blank value", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: " \n\t "}}, want: ""},
		{name: "OAuth value trims outer whitespace", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "  first\nsecond  "}}, want: "first\nsecond"},
		{name: "setup token value trims outer whitespace", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "  first\nsecond  "}}, want: "first\nsecond"},
		{name: "API key value trims outer whitespace", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "\n first\nsecond \t"}}, want: "first\nsecond"},
		{name: "unsupported OpenAI type", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeUpstream, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "instructions"}}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.GetOpenAICustomInstructions())
		})
	}
}

func TestValidateOpenAICustomInstructionsCredentials(t *testing.T) {
	tests := []struct {
		name        string
		platform    string
		accountType string
		credentials map[string]any
		wantReason  string
	}{
		{name: "absent value", platform: PlatformOpenAI, accountType: AccountTypeOAuth, credentials: map[string]any{}},
		{name: "nil value", platform: PlatformOpenAI, accountType: AccountTypeOAuth, credentials: map[string]any{OpenAICustomInstructionsCredentialKey: nil}},
		{name: "blank value", platform: PlatformOpenAI, accountType: AccountTypeOAuth, credentials: map[string]any{OpenAICustomInstructionsCredentialKey: " \n\t "}},
		{name: "setup token value", platform: PlatformOpenAI, accountType: AccountTypeSetupToken, credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "instructions"}},
		{name: "non string value", platform: PlatformOpenAI, accountType: AccountTypeOAuth, credentials: map[string]any{OpenAICustomInstructionsCredentialKey: 42}, wantReason: "OPENAI_CUSTOM_INSTRUCTIONS_INVALID"},
		{name: "non OpenAI usage", platform: PlatformGemini, accountType: AccountTypeOAuth, credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "instructions"}, wantReason: "OPENAI_CUSTOM_INSTRUCTIONS_UNSUPPORTED_PLATFORM"},
		{name: "unsupported OpenAI type", platform: PlatformOpenAI, accountType: AccountTypeUpstream, credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "instructions"}, wantReason: "OPENAI_CUSTOM_INSTRUCTIONS_UNSUPPORTED_ACCOUNT_TYPE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOpenAICustomInstructionsCredentials(tt.platform, tt.accountType, tt.credentials)
			if tt.wantReason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
		})
	}
}

func TestValidateOpenAICustomInstructionsCredentialsRejectsOversize(t *testing.T) {
	err := ValidateOpenAICustomInstructionsCredentials(PlatformOpenAI, AccountTypeOAuth, map[string]any{
		OpenAICustomInstructionsCredentialKey: strings.Repeat("界", OpenAICustomInstructionsMaxBytes),
	})

	require.Error(t, err)
	require.Equal(t, "OPENAI_CUSTOM_INSTRUCTIONS_TOO_LONG", infraerrors.Reason(err))
	require.NotContains(t, err.Error(), "界界界")
}

func TestValidateOpenAICustomInstructionsCredentialsRejectsOversizeBlankValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "ASCII whitespace", value: strings.Repeat(" ", OpenAICustomInstructionsMaxBytes+1)},
		{name: "Unicode whitespace", value: strings.Repeat("\u3000", OpenAICustomInstructionsMaxBytes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOpenAICustomInstructionsCredentials(PlatformOpenAI, AccountTypeOAuth, map[string]any{
				OpenAICustomInstructionsCredentialKey: tt.value,
			})

			require.Error(t, err)
			require.Equal(t, "OPENAI_CUSTOM_INSTRUCTIONS_TOO_LONG", infraerrors.Reason(err))
		})
	}
}
