package service

import (
	"encoding/json"
	"errors"
	"strings"
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

func TestRedactOpenAIAccountInstructionsFromEscapedJSONBody(t *testing.T) {
	account := customInstructionsAccount("line/中\nquote\"")
	body := []byte(`{"error":{"message":"prefix line\/\u4e2d\nquote\" suffix"}}`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamBody(account, body)

	require.NotContains(t, string(redacted), `line\/\u4e2d\nquote\"`)
	require.NotContains(t, gjson.GetBytes(redacted, "error.message").String(), account.GetOpenAICustomInstructions())
	require.Contains(t, gjson.GetBytes(redacted, "error.message").String(), openAIAccountInstructionsRedaction)
}

func TestRedactOpenAIAccountInstructionsFromJSONWithTrailingText(t *testing.T) {
	account := customInstructionsAccount("account suffix")
	body := []byte(`{"error":"safe"} echoed account suffix`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamBody(account, body)

	require.Equal(t, `{"error":"safe"} echoed `+openAIAccountInstructionsRedaction, string(redacted))
	require.NotContains(t, string(redacted), account.GetOpenAICustomInstructions())
}

func TestRedactOpenAIAccountInstructionsFromEveryDuplicateJSONKeyAndValue(t *testing.T) {
	account := customInstructionsAccount("account 中 suffix")
	body := []byte(`{"message":"account \u4e2d suffix","message":"safe","account \u4e2d suffix":"first","account 中 suffix":"second"}`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamBody(account, body)

	require.True(t, json.Valid(redacted))
	require.JSONEq(t, `{"message":"[redacted account instructions]","message":"safe","[redacted account instructions]":"first","[redacted account instructions]":"second"}`, string(redacted))
	require.NotContains(t, string(redacted), account.GetOpenAICustomInstructions())
	require.NotContains(t, string(redacted), `account \u4e2d suffix`)
	require.Equal(t, 3, strings.Count(string(redacted), openAIAccountInstructionsRedaction))
}

func TestRedactOpenAIAccountInstructionsLeavesNonmatchingDuplicateJSONUnchanged(t *testing.T) {
	account := customInstructionsAccount("account suffix")
	body := []byte(" {\n  \"message\" : \"safe\", \"message\" : \"still safe\", \"escaped\" : \"\\u4e2d\"\n} \n")

	redacted := redactOpenAIAccountInstructionsFromUpstreamBody(account, body)

	require.Equal(t, body, redacted)
}

func TestRedactOpenAIAccountInstructionsFromEscapedNonJSONError(t *testing.T) {
	account := customInstructionsAccount("line/中\nquote\"")
	err := errors.New(`status = policy and reason = "prefix line\/\u4e2d\nquote\" suffix"`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamError(account, err)

	require.NotContains(t, redacted.Error(), `line\/\u4e2d\nquote\"`)
	require.NotContains(t, redacted.Error(), account.GetOpenAICustomInstructions())
	require.Contains(t, redacted.Error(), openAIAccountInstructionsRedaction)
}

func TestRedactOpenAIAccountInstructionsFromLiteralBackslashSequencesInNonJSONError(t *testing.T) {
	for _, suffix := range []string{
		`Use \n literally`,
		`Use \t literally`,
		`Use \" literally`,
		`Use \/ literally`,
		`Use \\ literally`,
		`Use \u4E2D literally`,
	} {
		t.Run(suffix, func(t *testing.T) {
			account := customInstructionsAccount(suffix)
			err := errors.New(`websocket close: reason="prefix ` + suffix + ` suffix"`)

			redacted := redactOpenAIAccountInstructionsFromUpstreamError(account, err)

			require.NotContains(t, redacted.Error(), suffix)
			require.Contains(t, redacted.Error(), openAIAccountInstructionsRedaction)
		})
	}
}

func TestRedactOpenAIAccountInstructionsDoesNotTreatJSONEscapeAsLiteralSuffix(t *testing.T) {
	account := customInstructionsAccount(`\n`)
	body := []byte(`{"error":{"message":"unrelated line\nbreak"}}`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamBody(account, body)

	require.Equal(t, body, redacted)
}

func TestRedactOpenAIAccountInstructionsFromMixedJSONEscapesInNonJSONError(t *testing.T) {
	account := customInstructionsAccount("中<&😀")
	tests := []struct {
		name    string
		encoded string
	}{
		{
			name:    "uppercase hex and surrogate pair",
			encoded: `\u4E2D\u003c\u0026\ud83d\ude00`,
		},
		{
			name:    "mixed literal and escaped units",
			encoded: `中\u003C&\uD83D\uDE00`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := errors.New(`websocket close: reason="prefix ` + tt.encoded + ` suffix"`)

			redacted := redactOpenAIAccountInstructionsFromUpstreamError(account, err)

			require.NotContains(t, redacted.Error(), tt.encoded)
			require.NotContains(t, redacted.Error(), account.GetOpenAICustomInstructions())
			require.Contains(t, redacted.Error(), openAIAccountInstructionsRedaction)
		})
	}
}

func TestRedactOpenAIAccountInstructionsFromMixedEscapedSpecialCharacters(t *testing.T) {
	account := customInstructionsAccount("quote\"/\nnext")
	err := errors.New(`websocket close: reason="prefix quote\"\/\nnext suffix"`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamError(account, err)

	require.NotContains(t, redacted.Error(), `quote\"\/\nnext`)
	require.NotContains(t, redacted.Error(), account.GetOpenAICustomInstructions())
	require.Contains(t, redacted.Error(), openAIAccountInstructionsRedaction)
}

func TestRedactOpenAIAccountInstructionsDoesNotRedactNearSemanticMatch(t *testing.T) {
	account := customInstructionsAccount("中<&😀")
	err := errors.New(`websocket close: reason="prefix \u4E2D\u003c\u0026\ud83d\ude01 suffix"`)

	redacted := redactOpenAIAccountInstructionsFromUpstreamError(account, err)

	require.Equal(t, err, redacted)
	require.NotContains(t, redacted.Error(), openAIAccountInstructionsRedaction)
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
