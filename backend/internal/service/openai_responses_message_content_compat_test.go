package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIResponsesMessageContentForAccount(t *testing.T) {
	account := openAIResponsesAssistantPrefillCompatAccount(true)
	body := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"question"},
				{"type":"input_text"},
				{"type":"input_image","image_url":"data:image/png;base64,abc"}
			]},
			{"type":"message","role":"assistant","content":[
				{"type":"output_text"},
				{"type":"output_text","text":"   "},
				{"type":"output_text","text":"answer"},
				{"type":"refusal","refusal":"not allowed"}
			]},
			{"type":"message","role":"assistant","content":[]},
			{"role":"user","content":"   "},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	got, changed, err := normalizeOpenAIResponsesMessageContentForAccount(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"question"},
				{"type":"input_image","image_url":"data:image/png;base64,abc"}
			]},
			{"type":"message","role":"assistant","content":[
				{"type":"output_text","text":"answer"},
				{"type":"output_text","text":"not allowed"}
			]},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`, string(got))
}

func TestNormalizeOpenAIResponsesMessageContentDropsMissingAndNullContent(t *testing.T) {
	account := openAIResponsesMessagePartialCompatAccount(true)
	body := []byte(`{"input":[
		{"type":"message","role":"assistant"},
		{"type":"message","role":"assistant","content":null},
		{"type":"message","role":"user","content":"next"}
	]}`)

	got, changed, err := normalizeOpenAIResponsesMessageContentForAccount(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"input":[{"type":"message","role":"user","content":"next"}]}`, string(got))
}

func TestNormalizeOpenAIResponsesMessageContentNoop(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		body    string
	}{
		{name: "compat disabled", account: openAIResponsesAssistantPrefillCompatAccount(false), body: `{"input":[{"role":"assistant","content":[{"type":"output_text"}]}]}`},
		{name: "string input", account: openAIResponsesAssistantPrefillCompatAccount(true), body: `{"input":"hello"}`},
		{name: "valid content", account: openAIResponsesAssistantPrefillCompatAccount(true), body: `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := normalizeOpenAIResponsesMessageContentForAccount(tt.account, []byte(tt.body))
			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, tt.body, string(got))
		})
	}
}

func TestNormalizeOpenAIResponsesMessageContentRejectsInvalidJSON(t *testing.T) {
	_, _, err := normalizeOpenAIResponsesMessageContentForAccount(
		openAIResponsesAssistantPrefillCompatAccount(true),
		[]byte(`{"input":[`),
	)
	require.ErrorContains(t, err, "invalid request JSON")
}
