package service

import (
	"bytes"
	"context"
	"fmt"
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

func TestForwardNativeResponsesAccountCustomInstructionsRedactsUpstreamEcho(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tt := range []struct {
		name  string
		extra map[string]any
	}{
		{name: "normal"},
		{name: "passthrough", extra: map[string]any{"openai_passthrough": true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const suffix = "ACCOUNT-INSTRUCTION-SECRET"
			body := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`)
			account := openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, tt.extra)
			recorder, c := accountInstructionsErrorContext(t, "/v1/responses", body)
			upstream := accountInstructionsEchoingErrorRecorder(suffix)
			svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

			result, err := svc.Forward(context.Background(), c, account, body)

			require.Error(t, err)
			require.Nil(t, result)
			assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
		})
	}
}

func TestChatCompletionsBridgeAccountCustomInstructionsRedactsUpstreamEcho(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT-INSTRUCTION-SECRET"
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"client instructions"},{"role":"user","content":"hello"}],"stream":false}`)
	account := openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil)
	recorder, c := accountInstructionsErrorContext(t, "/v1/chat/completions", body)
	upstream := accountInstructionsEchoingErrorRecorder(suffix)
	svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")

	require.Error(t, err)
	require.Nil(t, result)
	assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
}

func TestAccountCustomInstructionsRedactsFailoverDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT-INSTRUCTION-SECRET"
	responsesBody := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`)
	chatBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"client instructions"},{"role":"user","content":"hello"}],"stream":false}`)
	tests := []struct {
		name    string
		path    string
		body    []byte
		account *Account
		forward func(*OpenAIGatewayService, *gin.Context, *Account, []byte) (*OpenAIForwardResult, error)
	}{
		{
			name:    "native responses",
			path:    "/v1/responses",
			body:    responsesBody,
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.Forward(context.Background(), c, account, body)
			},
		},
		{
			name: "passthrough",
			path: "/v1/responses",
			body: responsesBody,
			account: openAIAccountForCustomInstructionsIntegration(
				AccountTypeAPIKey,
				suffix,
				map[string]any{"openai_passthrough": true, "openai_responses_supported": true},
			),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.Forward(context.Background(), c, account, body)
			},
		},
		{
			name:    "chat completions bridge",
			path:    "/v1/chat/completions",
			body:    chatBody,
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, c := accountInstructionsErrorContext(t, tt.path, tt.body)
			upstream := accountInstructionsEchoingErrorRecorderWithStatus(http.StatusServiceUnavailable, suffix)
			svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

			result, err := tt.forward(svc, c, tt.account, tt.body)

			require.Error(t, err)
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.NotContains(t, string(failoverErr.ResponseBody), suffix, "failover body must not expose account instructions")
			assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
		})
	}
}

func TestForwardAccountCustomInstructionsReachSelectedUpstreamResponsesWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_account_instructions","model":"gpt-5.4","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: captureConn})
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := openAIAccountForCustomInstructionsIntegration(
		AccountTypeAPIKey,
		"account suffix",
		map[string]any{"responses_websockets_v2_enabled": true},
	)

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "client instructions\n\naccount suffix", captureConn.lastWrite["instructions"])
	decision, _ := c.Get("openai_ws_transport_decision")
	require.Equal(t, string(OpenAIUpstreamTransportResponsesWebsocketV2), decision)
}

func TestForwardAccountCustomInstructionsRedactsSelectedUpstreamResponsesWebSocketError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT-INSTRUCTION-SECRET"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	event := fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":%q}}`, "echoed account instructions: "+suffix)
	captureConn := &openAIWSCaptureConn{events: [][]byte{[]byte(event)}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: captureConn})
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := openAIAccountForCustomInstructionsIntegration(
		AccountTypeAPIKey,
		suffix,
		map[string]any{"responses_websockets_v2_enabled": true},
	)

	result, err := svc.Forward(context.Background(), c, account, body)

	require.Error(t, err)
	require.Nil(t, result)
	assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
}

func accountInstructionsErrorContext(t *testing.T, path string, body []byte) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return recorder, c
}

func accountInstructionsEchoingErrorRecorder(suffix string) *httpUpstreamRecorder {
	return accountInstructionsEchoingErrorRecorderWithStatus(http.StatusBadRequest, suffix)
}

func accountInstructionsEchoingErrorRecorderWithStatus(statusCode int, suffix string) *httpUpstreamRecorder {
	message := "Your input exceeds the context window; echoed account instructions: " + suffix
	if statusCode >= http.StatusInternalServerError {
		message = "Temporary upstream failure; echoed account instructions: " + suffix
	}
	responseBody := fmt.Sprintf(`{"error":{"type":"invalid_request_error","message":%q}}`, message)
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_account_instructions_echo"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}}
}

func accountInstructionsErrorTestConfig() *config.Config {
	return &config.Config{Gateway: config.GatewayConfig{
		LogUpstreamErrorBody:         true,
		LogUpstreamErrorBodyMaxBytes: 4096,
	}}
}

func assertAccountInstructionsNotExposed(
	t *testing.T,
	c *gin.Context,
	recorder *httptest.ResponseRecorder,
	err error,
	suffix string,
) {
	t.Helper()
	require.NotContains(t, recorder.Body.String(), suffix, "client response must not expose account instructions")
	require.NotContains(t, err.Error(), suffix, "returned error must not expose account instructions")
	for _, key := range []string{OpsUpstreamErrorMessageKey, OpsUpstreamErrorDetailKey} {
		if value, ok := c.Get(key); ok {
			require.NotContains(t, fmt.Sprint(value), suffix, "ops context must not expose account instructions")
		}
	}
	if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
		events, castOK := value.([]*OpsUpstreamErrorEvent)
		require.True(t, castOK)
		for _, event := range events {
			require.NotContains(t, fmt.Sprintf("%+v", event), suffix, "ops event must not expose account instructions")
		}
	}
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
