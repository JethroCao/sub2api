package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
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

func TestRedactOpenAIAccountInstructionsReplacementCannotContainConfiguredSuffix(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
		body   string
	}{
		{name: "exact former marker", suffix: openAIAccountInstructionsRedaction, body: `raw [redacted account instructions], escaped [redacted\u0020account instructions], duplicate [redacted account instructions]`},
		{name: "former marker substring", suffix: "account instructions", body: `raw account instructions and escaped account\u0020instructions`},
		{name: "single character", suffix: "a", body: `raw a and escaped \u0061 and duplicate a`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := customInstructionsAccount(tt.suffix)
			got := redactOpenAIAccountInstructionsFromUpstreamBody(account, []byte(tt.body))
			require.NotContains(t, string(got), tt.suffix)
		})
	}
}

func TestRedactOpenAIAccountInstructionsValidJSONRemainsValidForMarkerAndShortSuffix(t *testing.T) {
	for _, suffix := range []string{openAIAccountInstructionsRedaction, "a"} {
		t.Run(suffix, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"raw": suffix + suffix, "escaped": "prefix " + suffix})
			require.NoError(t, err)
			got := redactOpenAIAccountInstructionsFromUpstreamBody(customInstructionsAccount(suffix), body)
			require.True(t, json.Valid(got))
			require.NotContains(t, string(got), suffix)
			require.NotContains(t, gjson.GetBytes(got, "raw").String(), suffix)
		})
	}
}

func TestRedactOpenAIAccountInstructionsPreservesWebSocketCloseIdentity(t *testing.T) {
	suffix := "private close reason"
	original := coderws.CloseError{Code: coderws.StatusNormalClosure, Reason: "done: " + suffix}
	got := redactOpenAIAccountInstructionsFromUpstreamError(customInstructionsAccount(suffix), original)

	var closeErr coderws.CloseError
	require.ErrorAs(t, got, &closeErr)
	require.Equal(t, coderws.StatusNormalClosure, closeErr.Code)
	require.Equal(t, coderws.StatusNormalClosure, coderws.CloseStatus(got))
	require.True(t, isOpenAIWSClientDisconnectError(got), "sanitized normal closure must remain graceful")
	require.NotContains(t, closeErr.Reason, suffix)
	require.NotContains(t, got.Error(), suffix)
}

func TestRedactOpenAIAccountInstructionsPreservesWrappedSentinelClassification(t *testing.T) {
	sentinel := errors.New("classified sentinel")
	suffix := "private transport detail"
	original := fmt.Errorf("write failed: %s: %w", suffix, sentinel)
	got := redactOpenAIAccountInstructionsFromUpstreamError(customInstructionsAccount(suffix), original)

	require.ErrorIs(t, got, sentinel)
	require.NotContains(t, got.Error(), suffix)
}

func TestRedactOpenAIAccountInstructionsPreservesCancellationAndTimeoutClassification(t *testing.T) {
	const suffix = "private cancellation detail"
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		original := fmt.Errorf("upstream failed: %s: %w", suffix, sentinel)
		got := redactOpenAIAccountInstructionsFromUpstreamError(customInstructionsAccount(suffix), original)
		require.ErrorIs(t, got, sentinel)
		require.NotContains(t, got.Error(), suffix)
	}
}

func TestRedactOpenAIAccountInstructionsAdversarialRepeatedPrefixCompletesLinearly(t *testing.T) {
	suffix := strings.Repeat("a", 4095) + "b"
	body := strings.Repeat("a", 128<<10)
	started := time.Now()
	got := redactOpenAIAccountInstructionsFromUpstreamBody(customInstructionsAccount(suffix), []byte(body))
	require.Equal(t, body, string(got))
	require.Less(t, time.Since(started), 2*time.Second)
}

func BenchmarkRedactOpenAIAccountInstructionsRepeatedPrefix(b *testing.B) {
	suffix := strings.Repeat("a", 4095) + "b"
	body := []byte(strings.Repeat("a", 1<<20))
	account := customInstructionsAccount(suffix)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = redactOpenAIAccountInstructionsFromUpstreamBody(account, body)
	}
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
