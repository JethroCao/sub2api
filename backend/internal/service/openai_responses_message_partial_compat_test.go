package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func openAIResponsesMessagePartialCompatAccount(enabled any) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			OpenAIResponsesMessagePartialCompatExtraKey: enabled,
		},
	}
}

func TestAccountShouldEnableOpenAIResponsesMessagePartialCompat(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil account"},
		{name: "missing extra", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}},
		{name: "api key enabled", account: openAIResponsesMessagePartialCompatAccount(true), want: true},
		{name: "api key disabled", account: openAIResponsesMessagePartialCompatAccount(false)},
		{name: "wrong value type", account: openAIResponsesMessagePartialCompatAccount("true")},
		{
			name: "oauth ignores setting",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					OpenAIResponsesMessagePartialCompatExtraKey: true,
				},
			},
		},
		{
			name: "non openai ignores setting",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Extra: map[string]any{
					OpenAIResponsesMessagePartialCompatExtraKey: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.ShouldEnableOpenAIResponsesMessagePartialCompat())
		})
	}
}

func TestNormalizeOpenAIResponsesMessagePartialForAccount(t *testing.T) {
	account := openAIResponsesMessagePartialCompatAccount(true)
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","role":"user","content":"first"},
			{"type":"reasoning","encrypted_content":"cipher"},
			{"type":"message","role":"assistant","content":[],"partial":false},
			{"type":"message","role":"developer","content":"done","partial":false},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	got, changed, err := normalizeOpenAIResponsesMessagePartialForAccount(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","role":"user","content":"first"},
			{"type":"reasoning","encrypted_content":"cipher"},
			{"type":"message","role":"assistant","content":[]},
			{"type":"message","role":"developer","content":"done"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`, string(got))
}

func TestNormalizeOpenAIResponsesMessagePartialForAccountUsesLastMessage(t *testing.T) {
	account := openAIResponsesMessagePartialCompatAccount(true)
	body := []byte(`{
		"input":[
			{"type":"message","role":"assistant","content":"older","partial":true},
			{"type":"message","role":"user","content":"next","partial":true},
			{"type":"message","role":"assistant","content":"latest"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	got, changed, err := normalizeOpenAIResponsesMessagePartialForAccount(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"input":[
			{"type":"message","role":"assistant","content":"older"},
			{"type":"message","role":"user","content":"next"},
			{"type":"message","role":"assistant","content":"latest","partial":true},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`, string(got))
}

func TestNormalizeOpenAIResponsesMessagePartialForAccountNoop(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		body    string
	}{
		{
			name:    "disabled",
			account: openAIResponsesMessagePartialCompatAccount(false),
			body:    `{"input":[{"type":"message","role":"user","content":"hello"}]}`,
		},
		{
			name:    "string input",
			account: openAIResponsesMessagePartialCompatAccount(true),
			body:    `{"input":"hello"}`,
		},
		{
			name:    "non-assistant message without partial",
			account: openAIResponsesMessagePartialCompatAccount(true),
			body:    `{"input":[{"type":"message","role":"user","content":"hello"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := normalizeOpenAIResponsesMessagePartialForAccount(tt.account, []byte(tt.body))
			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, tt.body, string(got))
		})
	}
}

func TestNormalizeOpenAIResponsesMessagePartialForAccountRejectsInvalidJSON(t *testing.T) {
	_, _, err := normalizeOpenAIResponsesMessagePartialForAccount(
		openAIResponsesMessagePartialCompatAccount(true),
		[]byte(`{"input":[`),
	)
	require.ErrorContains(t, err, "invalid request JSON")
}
