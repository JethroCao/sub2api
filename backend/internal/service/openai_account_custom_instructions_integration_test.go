package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardNativeResponsesAccountCustomInstructions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name             string
		path             string
		body             []byte
		account          *Account
		wantInstructions string
		wantBody         []byte
	}{
		{
			name: "OAuth stream false",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5.4","stream":false,"instructions":"  client instructions  ","input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(
				AccountTypeOAuth,
				"account suffix",
				nil,
			),
			wantInstructions: "  client instructions  \n\naccount suffix",
		},
		{
			name: "API key stream true",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(
				AccountTypeAPIKey,
				"account suffix",
				map[string]any{"openai_responses_supported": true},
			),
			wantInstructions: "client instructions\n\naccount suffix",
		},
		{
			name: "passthrough",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(
				AccountTypeOAuth,
				"account suffix",
				map[string]any{"openai_passthrough": true},
			),
			wantInstructions: "client instructions\n\naccount suffix",
		},
		{
			name: "compact",
			path: "/v1/responses/compact",
			body: []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(
				AccountTypeOAuth,
				"account suffix",
				nil,
			),
			wantInstructions: "client instructions\n\naccount suffix",
		},
		{
			name: "unconfigured passthrough preserves bytes",
			path: "/v1/responses",
			body: []byte(` {"model":"gpt-5.4", "stream":false, "instructions":"client instructions", "input":"hello"} `),
			account: openAIAccountForCustomInstructionsIntegration(
				AccountTypeAPIKey,
				"",
				map[string]any{"openai_passthrough": true, "openai_responses_supported": true},
			),
			wantBody: []byte(` {"model":"gpt-5.4", "stream":false, "instructions":"client instructions", "input":"hello"} `),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamBody := forwardNativeResponsesForAccountInstructionsTest(t, tt.path, tt.body, tt.account)
			if tt.wantBody != nil {
				require.Equal(t, tt.wantBody, upstreamBody)
				return
			}
			require.Equal(t, tt.wantInstructions, gjson.GetBytes(upstreamBody, "instructions").String())
		})
	}
}

func TestChatCompletionsBridgeAccountCustomInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"converted system instructions"},{"role":"user","content":"hello"}],"stream":false}`)
	account := openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, "account suffix", nil)

	upstreamBody := forwardChatCompletionsForAccountInstructionsTest(t, body, account)

	require.Equal(t, "converted system instructions\n\naccount suffix", gjson.GetBytes(upstreamBody, "instructions").String())
}

func forwardNativeResponsesForAccountInstructionsTest(t *testing.T, path string, body []byte, account *Account) []byte {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	upstream := accountInstructionsHTTPRecorder()
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.NotEmpty(t, upstream.lastBody)
	return upstream.lastBody
}

func forwardChatCompletionsForAccountInstructionsTest(t *testing.T, body []byte, account *Account) []byte {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := accountInstructionsHTTPRecorder()
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")
	require.Error(t, err)
	require.Nil(t, result)
	require.NotEmpty(t, upstream.lastBody)
	return upstream.lastBody
}

func accountInstructionsHTTPRecorder() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_account_instructions"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop after recording request"}}`)),
	}}
}

func openAIAccountForCustomInstructionsIntegration(accountType string, instructions string, extra map[string]any) *Account {
	credentials := map[string]any{OpenAICustomInstructionsCredentialKey: instructions}
	if accountType == AccountTypeOAuth {
		credentials["access_token"] = "oauth-token"
		credentials["chatgpt_account_id"] = "chatgpt-account"
	} else {
		credentials["api_key"] = "sk-test"
	}
	return &Account{
		ID:          301,
		Name:        "account-instructions-integration",
		Platform:    PlatformOpenAI,
		Type:        accountType,
		Concurrency: 1,
		Credentials: credentials,
		Extra:       extra,
		Status:      StatusActive,
		Schedulable: true,
	}
}
