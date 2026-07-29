package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func openAIResponsesAssistantPrefillCompatAccount(enabled any) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			OpenAIResponsesAssistantPrefillCompatExtraKey: enabled,
		},
	}
}

func TestAccountShouldEnableOpenAIResponsesAssistantPrefillCompat(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil account"},
		{name: "missing extra", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}},
		{name: "api key enabled", account: openAIResponsesAssistantPrefillCompatAccount(true), want: true},
		{name: "api key disabled", account: openAIResponsesAssistantPrefillCompatAccount(false)},
		{name: "wrong value type", account: openAIResponsesAssistantPrefillCompatAccount("true")},
		{
			name: "oauth ignores setting",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
				OpenAIResponsesAssistantPrefillCompatExtraKey: true,
			}},
		},
		{
			name: "non openai ignores setting",
			account: &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
				OpenAIResponsesAssistantPrefillCompatExtraKey: true,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.ShouldEnableOpenAIResponsesAssistantPrefillCompat())
		})
	}
}

func TestNormalizeOpenAIResponsesAssistantPrefillForAccount(t *testing.T) {
	account := openAIResponsesAssistantPrefillCompatAccount(true)
	body := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":"question"},
			{"type":"reasoning","encrypted_content":"cipher"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"draft"}],"partial":true}
		]
	}`)

	got, changed, err := normalizeOpenAIResponsesAssistantPrefillForAccount(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":"question"},
			{"type":"reasoning","encrypted_content":"cipher"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"draft"}],"partial":true},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}
		]
	}`, string(got))
}

func TestNormalizeOpenAIResponsesAssistantPrefillRecognizesEasyInputMessage(t *testing.T) {
	account := openAIResponsesAssistantPrefillCompatAccount(true)
	body := []byte(`{"input":[{"role":"assistant","content":"draft"}]}`)

	got, changed, err := normalizeOpenAIResponsesAssistantPrefillForAccount(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"input":[
		{"role":"assistant","content":"draft"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}
	]}`, string(got))

	gotAgain, changedAgain, err := normalizeOpenAIResponsesAssistantPrefillForAccount(account, got)
	require.NoError(t, err)
	require.False(t, changedAgain)
	require.JSONEq(t, string(got), string(gotAgain))
}

func TestNormalizeOpenAIResponsesAssistantPrefillNoop(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		body    string
	}{
		{name: "disabled", account: openAIResponsesAssistantPrefillCompatAccount(false), body: `{"input":[{"role":"assistant","content":"draft"}]}`},
		{name: "string input", account: openAIResponsesAssistantPrefillCompatAccount(true), body: `{"input":"hello"}`},
		{name: "empty input", account: openAIResponsesAssistantPrefillCompatAccount(true), body: `{"input":[]}`},
		{name: "last user", account: openAIResponsesAssistantPrefillCompatAccount(true), body: `{"input":[{"role":"assistant","content":"draft"},{"role":"user","content":"next"}]}`},
		{name: "trailing tool output", account: openAIResponsesAssistantPrefillCompatAccount(true), body: `{"input":[{"role":"assistant","content":"draft"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := normalizeOpenAIResponsesAssistantPrefillForAccount(tt.account, []byte(tt.body))
			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, tt.body, string(got))
		})
	}
}

func TestNormalizeOpenAIResponsesAssistantPrefillRejectsInvalidJSON(t *testing.T) {
	_, _, err := normalizeOpenAIResponsesAssistantPrefillForAccount(
		openAIResponsesAssistantPrefillCompatAccount(true),
		[]byte(`{"input":[`),
	)
	require.ErrorContains(t, err, "invalid request JSON")
}
