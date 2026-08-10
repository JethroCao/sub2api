package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIResponsesPartialMissingRetryBodySetsLastAssistantBeforeToolSuffix(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"assistant","content":"older"},
		{"type":"message","role":"assistant","content":"draft"},
		{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"{}"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}
	]}`)
	errorBody := []byte(`{"error":{"code":"MissingParameter","message":"missing partial parameter","param":"partial"}}`)

	got, changed, err := normalizeOpenAIResponsesPartialMissingRetryBody(
		openAIResponsesMessagePartialCompatAccount(true),
		http.StatusBadRequest,
		"missing partial parameter",
		body,
		errorBody,
	)

	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(got, "input.0.partial").Exists())
	require.True(t, gjson.GetBytes(got, "input.1.partial").Bool())
	require.False(t, gjson.GetBytes(got, "input.2.partial").Exists())
	require.False(t, gjson.GetBytes(got, "input.3.partial").Exists())
}

func TestNormalizeOpenAIResponsesPartialMissingRetryBodyNoop(t *testing.T) {
	errorBody := []byte(`{"error":{"code":"MissingParameter","message":"missing partial parameter","param":"partial"}}`)
	tests := []struct {
		name         string
		account      *Account
		statusCode   int
		upstreamMsg  string
		requestBody  string
		upstreamBody []byte
	}{
		{
			name:         "compatibility disabled",
			account:      openAIResponsesMessagePartialCompatAccount(false),
			statusCode:   http.StatusBadRequest,
			upstreamMsg:  "missing partial parameter",
			requestBody:  `{"input":[{"type":"message","role":"assistant","content":"draft"}]}`,
			upstreamBody: errorBody,
		},
		{
			name:         "different upstream error",
			account:      openAIResponsesMessagePartialCompatAccount(true),
			statusCode:   http.StatusBadRequest,
			upstreamMsg:  "missing model parameter",
			requestBody:  `{"input":[{"type":"message","role":"assistant","content":"draft"}]}`,
			upstreamBody: []byte(`{"error":{"code":"MissingParameter","message":"missing model parameter","param":"model"}}`),
		},
		{
			name:         "no assistant message",
			account:      openAIResponsesMessagePartialCompatAccount(true),
			statusCode:   http.StatusBadRequest,
			upstreamMsg:  "missing partial parameter",
			requestBody:  `{"input":[{"type":"message","role":"user","content":"hello"}]}`,
			upstreamBody: errorBody,
		},
		{
			name:         "last assistant already partial",
			account:      openAIResponsesMessagePartialCompatAccount(true),
			statusCode:   http.StatusBadRequest,
			upstreamMsg:  "missing partial parameter",
			requestBody:  `{"input":[{"type":"message","role":"assistant","content":"draft","partial":true},{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}]}`,
			upstreamBody: errorBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := normalizeOpenAIResponsesPartialMissingRetryBody(
				tt.account,
				tt.statusCode,
				tt.upstreamMsg,
				[]byte(tt.requestBody),
				tt.upstreamBody,
			)

			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, tt.requestBody, string(got))
		})
	}
}

func TestNormalizeOpenAIResponsesPartialMissingRetryBodyRejectsInvalidJSON(t *testing.T) {
	_, _, err := normalizeOpenAIResponsesPartialMissingRetryBody(
		openAIResponsesMessagePartialCompatAccount(true),
		http.StatusBadRequest,
		"missing partial parameter",
		[]byte(`{"input":[`),
		[]byte(`{"error":{"code":"MissingParameter","param":"partial"}}`),
	)

	require.ErrorContains(t, err, "invalid request JSON")
}
