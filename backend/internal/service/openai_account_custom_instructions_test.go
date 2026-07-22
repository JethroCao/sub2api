package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAppendOpenAIAccountInstructionsAddsSuffixWhenInstructionsAreMissing(t *testing.T) {
	got, changed, err := appendOpenAIAccountInstructions(customInstructionsAccount("account suffix"), []byte(`{"model":"gpt-5.2","input":"hi"}`))

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "account suffix", gjson.GetBytes(got, "instructions").String())
}

func TestAppendOpenAIAccountInstructionsAddsSuffixWhenInstructionsAreEmpty(t *testing.T) {
	got, changed, err := appendOpenAIAccountInstructions(customInstructionsAccount("account suffix"), []byte(`{"instructions":"  \n\t "}`))

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "account suffix", gjson.GetBytes(got, "instructions").String())
}

func TestAppendOpenAIAccountInstructionsPreservesClientInstructions(t *testing.T) {
	account := customInstructionsAccount("account suffix")
	got, changed, err := appendOpenAIAccountInstructions(account,
		[]byte(`{"model":"gpt-5.2","instructions":"client instructions","input":"hi"}`))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client instructions\n\naccount suffix", gjson.GetBytes(got, "instructions").String())
}

func TestAppendOpenAIAccountInstructionsPreservesClientInstructionBoundaryWhitespace(t *testing.T) {
	account := customInstructionsAccount("account suffix")
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "ASCII whitespace",
			body: `{"instructions":"  client instructions  "}`,
			want: "  client instructions  \n\naccount suffix",
		},
		{
			name: "Unicode whitespace",
			body: `{"instructions":"\u3000客户说明\u00a0"}`,
			want: "\u3000客户说明\u00a0\n\naccount suffix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := appendOpenAIAccountInstructions(account, []byte(tt.body))

			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, tt.want, gjson.GetBytes(got, "instructions").String())
		})
	}
}

func TestAppendOpenAIAccountInstructionsPreservesUnicode(t *testing.T) {
	got, changed, err := appendOpenAIAccountInstructions(customInstructionsAccount("账户说明：保持中文"), []byte(`{"instructions":"客户说明：保留 Unicode"}`))

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "客户说明：保留 Unicode\n\n账户说明：保持中文", gjson.GetBytes(got, "instructions").String())
}

func TestAppendOpenAIAccountInstructionsDoesNotAppendExactExistingSuffix(t *testing.T) {
	account := customInstructionsAccount("account suffix")
	body := []byte(`{"instructions":"client instructions\n\naccount suffix"}`)

	got, changed, err := appendOpenAIAccountInstructions(account, body)

	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestAppendOpenAIAccountInstructionsRejectsNonStringInstructions(t *testing.T) {
	body := []byte(`{"instructions":["client instructions"]}`)

	got, changed, err := appendOpenAIAccountInstructions(customInstructionsAccount("account suffix"), body)

	require.EqualError(t, err, "OpenAI instructions must be a string")
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestAppendOpenAIAccountInstructionsSkipsUnconfiguredAndNonOpenAIAccounts(t *testing.T) {
	body := []byte(`{"instructions":"client instructions"}`)
	accounts := []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		{Platform: PlatformGemini, Type: AccountTypeOAuth, Credentials: map[string]any{OpenAICustomInstructionsCredentialKey: "account suffix"}},
	}

	for _, account := range accounts {
		got, changed, err := appendOpenAIAccountInstructions(account, body)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, got)
	}
}

func TestAppendOpenAIAccountInstructionsUsesCurrentAccountForEachOriginalBody(t *testing.T) {
	body := []byte(`{"instructions":"client instructions"}`)

	gotA, changedA, errA := appendOpenAIAccountInstructions(customInstructionsAccount("account A"), body)
	gotB, changedB, errB := appendOpenAIAccountInstructions(customInstructionsAccount("account B"), body)

	require.NoError(t, errA)
	require.NoError(t, errB)
	require.True(t, changedA)
	require.True(t, changedB)
	require.Equal(t, "client instructions\n\naccount A", gjson.GetBytes(gotA, "instructions").String())
	require.Equal(t, "client instructions\n\naccount B", gjson.GetBytes(gotB, "instructions").String())
}

func customInstructionsAccount(instructions string) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			OpenAICustomInstructionsCredentialKey: instructions,
		},
	}
}
