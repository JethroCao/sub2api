package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
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
		[]byte(`{"type":"response.completed","response":{"id":"resp_account_instructions","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"account suffix"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`),
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
	require.Contains(t, recorder.Body.String(), "account suffix", "successful model output matching the suffix must remain unchanged")
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

func TestAccountCustomInstructionsRedactsStreamingFailureEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT/INSTRUCTION-SECRET"
	const escapedSuffix = `ACCOUNT\/INSTRUCTION-SECRET`
	failedEvent := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"input exceeds the context window; echoed account instructions: ` + escapedSuffix + `"},"usage":{"input_tokens":3,"output_tokens":1}}}`,
		"",
	}, "\n")

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
			body:    []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.Forward(context.Background(), c, account, body)
			},
		},
		{
			name: "passthrough",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`),
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
			body:    []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"system","content":"client instructions"},{"role":"user","content":"hello"}]}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, c := accountInstructionsErrorContext(t, tt.path, tt.body)
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"x-request-id": []string{"rid_account_instructions_stream_echo"},
				},
				Body: io.NopCloser(strings.NewReader(failedEvent)),
			}}
			svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

			_, err := tt.forward(svc, c, tt.account, tt.body)

			require.Error(t, err)
			assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
			require.NotContains(t, recorder.Body.String(), escapedSuffix, "escaped account instructions must not reach the client")
		})
	}
}

func TestAccountCustomInstructionsRedactsBareStreamingErrorEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT/INSTRUCTION-SECRET"
	const escapedSuffix = `ACCOUNT\/INSTRUCTION-SECRET`
	errorEvent := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","error":{"type":"server_error","code":"server_error","message":"temporary upstream failure; echoed account instructions: ` + escapedSuffix + `"}}`,
		"",
	}, "\n") + "\n"
	partialEvent := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
	}, "\n") + "\n"

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
			body:    []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.Forward(context.Background(), c, account, body)
			},
		},
		{
			name: "passthrough",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`),
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
			body:    []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"system","content":"client instructions"},{"role":"user","content":"hello"}]}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
				return svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")
			},
		},
	}

	sink, releaseLogs := captureStructuredLog(t)
	defer releaseLogs()
	for _, tt := range tests {
		for _, afterOutput := range []bool{false, true} {
			name := "before output"
			upstreamSSE := errorEvent
			if afterOutput {
				name = "after output"
				upstreamSSE = partialEvent + errorEvent
			}
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				recorder, c := accountInstructionsErrorContext(t, tt.path, tt.body)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"text/event-stream"},
						"x-request-id": []string{"rid_account_instructions_bare_error_echo"},
					},
					Body: io.NopCloser(strings.NewReader(upstreamSSE)),
				}}
				svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

				_, err := tt.forward(svc, c, tt.account, tt.body)

				require.Error(t, err)
				assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
				require.NotContains(t, recorder.Body.String(), escapedSuffix, "escaped account instructions must not reach the client")
				for _, key := range []string{OpsUpstreamErrorMessageKey, OpsUpstreamErrorDetailKey} {
					if value, ok := c.Get(key); ok {
						require.NotContains(t, fmt.Sprint(value), escapedSuffix, "escaped account instructions must not reach ops context")
					}
				}
				if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
					require.NotContains(t, fmt.Sprint(value), escapedSuffix, "escaped account instructions must not reach ops events")
				}
				if afterOutput {
					require.Contains(t, recorder.Body.String(), "partial", "prior successful output must be preserved")
					require.Contains(t, recorder.Body.String(), openAIAccountInstructionsRedaction, "redacted upstream error must reach the client")
					var failoverErr *UpstreamFailoverError
					require.False(t, errors.As(err, &failoverErr), "cannot fail over after client output")
					return
				}
				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr, "bare server errors before output must remain eligible for failover")
				require.NotContains(t, string(failoverErr.ResponseBody), suffix)
				require.NotContains(t, string(failoverErr.ResponseBody), escapedSuffix)
			})
		}
	}
	require.False(t, sink.ContainsMessage(suffix), "structured logs must not expose account instructions")
	for _, field := range []string{"error", "body", "detail", "message", "cause", "close_reason"} {
		require.False(t, sink.ContainsFieldValue(field, suffix), "structured log field %q must not expose account instructions", field)
		require.False(t, sink.ContainsFieldValue(field, escapedSuffix), "structured log field %q must not expose escaped account instructions", field)
	}
}

func TestAccountCustomInstructionsRedactsBufferedHTTP200Failures(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT/INSTRUCTION-SECRET"
	const escapedSuffix = `ACCOUNT\/INSTRUCTION-SECRET`
	directFailure := `{"id":"resp_failed","status":"failed","error":{"type":"server_error","code":"server_error","message":"temporary upstream failure; echoed account instructions: ` + escapedSuffix + `"},"usage":{"input_tokens":3,"output_tokens":1}}`
	responseFailedSSE := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"type":"server_error","code":"server_error","message":"temporary upstream failure; echoed account instructions: ` + escapedSuffix + `"},"usage":{"input_tokens":3,"output_tokens":1}}}`,
		"",
	}, "\n") + "\n"
	bareErrorSSE := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","error":{"type":"server_error","code":"server_error","message":"temporary upstream failure; echoed account instructions: ` + escapedSuffix + `"}}`,
		"",
	}, "\n") + "\n"

	forms := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "direct failed response", contentType: "application/json", body: directFailure},
		{name: "SSE response.failed fallback", contentType: "text/event-stream", body: responseFailedSSE},
		{name: "SSE bare error", contentType: "text/event-stream", body: bareErrorSSE},
	}
	paths := []struct {
		name    string
		account *Account
	}{
		{name: "native", account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil)},
		{name: "passthrough", account: openAIAccountForCustomInstructionsIntegration(AccountTypeAPIKey, suffix, map[string]any{"openai_passthrough": true, "openai_responses_supported": true})},
	}

	sink, releaseLogs := captureStructuredLog(t)
	defer releaseLogs()
	for _, path := range paths {
		for _, form := range forms {
			t.Run(path.name+"/"+form.name, func(t *testing.T) {
				requestBody := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`)
				recorder, c := accountInstructionsErrorContext(t, "/v1/responses", requestBody)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{form.contentType},
						"x-request-id": []string{"rid_account_instructions_buffered_failure"},
					},
					Body: io.NopCloser(strings.NewReader(form.body)),
				}}
				svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

				result, err := svc.Forward(context.Background(), c, path.account, requestBody)

				require.Error(t, err)
				require.Nil(t, result)
				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr, "buffered server errors must remain eligible for failover")
				assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
				require.NotContains(t, recorder.Body.String(), escapedSuffix)
				require.NotContains(t, string(failoverErr.ResponseBody), escapedSuffix)
			})
		}
	}
	require.False(t, sink.ContainsMessage(suffix), "structured logs must not expose account instructions")
	for _, field := range []string{"error", "body", "detail", "message", "cause", "close_reason"} {
		require.False(t, sink.ContainsFieldValue(field, suffix), "structured log field %q must not expose account instructions", field)
		require.False(t, sink.ContainsFieldValue(field, escapedSuffix), "structured log field %q must not expose escaped account instructions", field)
	}
}

func TestAccountCustomInstructionsPreservesSuccessfulBufferedOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT-INSTRUCTION-SECRET"
	requestBody := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"client instructions","input":"hello"}`)
	for _, tt := range []struct {
		name    string
		account *Account
	}{
		{name: "native", account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil)},
		{name: "passthrough", account: openAIAccountForCustomInstructionsIntegration(AccountTypeAPIKey, suffix, map[string]any{"openai_passthrough": true, "openai_responses_supported": true})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder, c := accountInstructionsErrorContext(t, "/v1/responses", requestBody)
			upstreamBody := `{"id":"resp_ok","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + suffix + `"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}}
			svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}

			result, err := svc.Forward(context.Background(), c, tt.account, requestBody)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, recorder.Body.String(), suffix, "successful model output matching the suffix must remain unchanged")
		})
	}
}

func TestAccountCustomInstructionsRedactsHTTPStreamingReadErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const suffix = "private 中\n suffix"
	const escapedSuffix = `private \u4e2d\n suffix`
	tests := []struct {
		name    string
		path    string
		body    []byte
		account *Account
		forward func(*OpenAIGatewayService, *gin.Context, *Account, []byte) (*OpenAIForwardResult, error)
	}{
		{
			name: "native streaming", path: "/v1/responses",
			body:    []byte(`{"model":"gpt-5.4","stream":true,"input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.Forward(context.Background(), c, a, b)
			},
		},
		{
			name: "passthrough streaming", path: "/v1/responses",
			body:    []byte(`{"model":"gpt-5.4","stream":true,"input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeAPIKey, suffix, map[string]any{"openai_passthrough": true, "openai_responses_supported": true}),
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.Forward(context.Background(), c, a, b)
			},
		},
		{
			name: "chat bridge streaming", path: "/v1/chat/completions",
			body:    []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.ForwardAsChatCompletions(context.Background(), c, a, b, "", "gpt-5.4")
			},
		},
		{
			name: "native buffered", path: "/v1/responses",
			body:    []byte(`{"model":"gpt-5.4","stream":false,"input":"hello"}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.Forward(context.Background(), c, a, b)
			},
		},
		{
			name: "chat bridge buffered", path: "/v1/chat/completions",
			body:    []byte(`{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"hello"}]}`),
			account: openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil),
			forward: func(s *OpenAIGatewayService, c *gin.Context, a *Account, b []byte) (*OpenAIForwardResult, error) {
				return s.ForwardAsChatCompletions(context.Background(), c, a, b, "", "gpt-5.4")
			},
		},
	}
	for _, tt := range tests {
		for _, errText := range []string{"upstream read failed: " + suffix, "upstream read failed: " + escapedSuffix} {
			t.Run(tt.name+"/"+fmt.Sprint(len(errText)), func(t *testing.T) {
				recorder, c := accountInstructionsErrorContext(t, tt.path, tt.body)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_read_error"}},
					Body:       &accountInstructionsErrTailReader{data: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"), err: errors.New(errText)},
				}}
				svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}
				_, err := tt.forward(svc, c, tt.account, tt.body)
				require.Error(t, err)
				assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
				require.NotContains(t, err.Error(), escapedSuffix)
			})
		}
	}
}

type accountInstructionsErrTailReader struct {
	data []byte
	err  error
}

func (r *accountInstructionsErrTailReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func (r *accountInstructionsErrTailReader) Close() error { return nil }

func TestChatCompletionsBufferedBareErrorRedactsAndFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT/INSTRUCTION-SECRET"
	const escapedSuffix = `ACCOUNT\/INSTRUCTION-SECRET`
	requestBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"client instructions"},{"role":"user","content":"hello"}],"stream":false}`)
	recorder, c := accountInstructionsErrorContext(t, "/v1/chat/completions", requestBody)
	upstreamSSE := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","error":{"type":"server_error","code":"server_error","message":"temporary upstream failure; echoed account instructions: ` + escapedSuffix + `"}}`,
		"",
	}, "\n") + "\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"rid_account_instructions_chat_buffered_error"},
		},
		Body: io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	svc := &OpenAIGatewayService{cfg: accountInstructionsErrorTestConfig(), httpUpstream: upstream}
	account := openAIAccountForCustomInstructionsIntegration(AccountTypeOAuth, suffix, nil)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, requestBody, "", "gpt-5.4")

	require.Error(t, err)
	require.Nil(t, result)
	require.NotContains(t, err.Error(), "missing terminal event")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
	require.NotContains(t, string(failoverErr.ResponseBody), escapedSuffix)
}

func TestAccountCustomInstructionsRedactsWebSocketPrewarmAndWriteErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT-INSTRUCTION-SECRET"
	account := openAIAccountForCustomInstructionsIntegration(
		AccountTypeAPIKey,
		suffix,
		map[string]any{"responses_websockets_v2_enabled": true},
	)
	payload := map[string]any{
		"type":         "response.create",
		"model":        "gpt-5.4",
		"instructions": "client instructions\n\n" + suffix,
	}
	sink, releaseLogs := captureStructuredLog(t)
	defer releaseLogs()

	for _, tt := range []struct {
		name      string
		prewarm   bool
		writeErr  error
		readErr   error
		readEvent []byte
	}{
		{
			name:     "prewarm write",
			prewarm:  true,
			writeErr: errors.New("prewarm write echoed account instructions: " + suffix),
		},
		{
			name:    "prewarm read close",
			prewarm: true,
			readErr: coderws.CloseError{Code: coderws.StatusPolicyViolation, Reason: "prewarm close echoed account instructions: " + suffix},
		},
		{
			name:      "prewarm error event",
			prewarm:   true,
			readEvent: []byte(fmt.Sprintf(`{"type":"error","error":{"type":"server_error","code":"server_error","message":%q}}`, "prewarm error echoed account instructions: "+suffix)),
		},
		{
			name:     "main request write",
			prewarm:  false,
			writeErr: errors.New("main write echoed account instructions: " + suffix),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := accountInstructionsWebSocketErrorTestConfig(tt.prewarm)
			svc := &OpenAIGatewayService{cfg: cfg, toolCorrector: NewCodexToolCorrector()}
			conn := &accountInstructionsWSStageErrorConn{
				writeErr: tt.writeErr,
				readErr:  tt.readErr,
			}
			if tt.readEvent != nil {
				conn.events = [][]byte{tt.readEvent}
			}
			lease := &openAIWSConnLease{
				accountID: account.ID,
				conn:      newOpenAIWSConn("account_instructions_error", account.ID, conn, nil),
			}

			var err error
			if tt.prewarm {
				err = svc.performOpenAIWSGeneratePrewarm(
					context.Background(),
					lease,
					OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
					payload,
					"",
					payload,
					account,
					nil,
					0,
				)
			} else {
				svc.openaiWSPool = &openAIWSConnPool{}
				err = accountInstructionsForwardWSMainWriteForTest(t, svc, cfg, account, conn, payload)
			}

			require.Error(t, err)
			require.NotContains(t, err.Error(), suffix, "returned websocket error must not expose account instructions")
		})
	}
	require.False(t, sink.ContainsMessage(suffix), "structured websocket logs must not expose account instructions")
	for _, field := range []string{"error", "body", "detail", "message", "cause", "close_reason"} {
		require.False(t, sink.ContainsFieldValue(field, suffix), "structured websocket log field %q must not expose account instructions", field)
	}
}

func TestAccountCustomInstructionsRedactsLiteralBackslashesFromWebSocketCloseAndWriteErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = `Use \n, \t, \", \/, \\, and \u4E2D literally`
	account := openAIAccountForCustomInstructionsIntegration(
		AccountTypeAPIKey,
		suffix,
		map[string]any{"responses_websockets_v2_enabled": true},
	)
	payload := map[string]any{
		"type":         "response.create",
		"model":        "gpt-5.4",
		"instructions": "client instructions\n\n" + suffix,
	}
	sink, releaseLogs := captureStructuredLog(t)
	defer releaseLogs()

	for _, tt := range []struct {
		name     string
		prewarm  bool
		writeErr error
		readErr  error
	}{
		{
			name:    "websocket close",
			prewarm: true,
			readErr: coderws.CloseError{Code: coderws.StatusPolicyViolation, Reason: "echoed account instructions: " + suffix},
		},
		{
			name:     "websocket write",
			writeErr: errors.New("echoed account instructions: " + suffix),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := accountInstructionsWebSocketErrorTestConfig(tt.prewarm)
			svc := &OpenAIGatewayService{cfg: cfg, toolCorrector: NewCodexToolCorrector()}
			conn := &accountInstructionsWSStageErrorConn{writeErr: tt.writeErr, readErr: tt.readErr}
			lease := &openAIWSConnLease{
				accountID: account.ID,
				conn:      newOpenAIWSConn("account_instructions_literal_backslash_error", account.ID, conn, nil),
			}

			var err error
			if tt.prewarm {
				err = svc.performOpenAIWSGeneratePrewarm(
					context.Background(),
					lease,
					OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
					payload,
					"",
					payload,
					account,
					nil,
					0,
				)
			} else {
				err = accountInstructionsForwardWSMainWriteForTest(t, svc, cfg, account, conn, payload)
			}

			require.Error(t, err)
			require.NotContains(t, err.Error(), suffix)
			require.Contains(t, err.Error(), openAIAccountInstructionsRedaction)
		})
	}
	require.False(t, sink.ContainsMessage(suffix), "structured websocket logs must not expose literal account instructions")
	for _, field := range []string{"error", "body", "detail", "message", "cause", "close_reason"} {
		require.False(t, sink.ContainsFieldValue(field, suffix), "structured websocket log field %q must not expose literal account instructions", field)
	}
}

func TestForwardAccountCustomInstructionsRedactsUpstreamWebSocketReadCloseError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const suffix = "ACCOUNT-INSTRUCTION-SECRET"
	sink, releaseLogs := captureStructuredLog(t)
	defer releaseLogs()

	for _, tt := range []struct {
		name       string
		events     [][]byte
		wantFBErr  bool
		wantOpsErr bool
	}{
		{name: "before output returns sanitized fallback", wantFBErr: true},
		{
			name: "after output returns sanitized error and ops detail",
			events: [][]byte{
				[]byte(`{"type":"response.output_text.delta","response_id":"resp_read_error","delta":"partial"}`),
			},
			wantOpsErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder, c, err := forwardAccountInstructionsWSReadErrorForTest(t, suffix, tt.events)

			require.Error(t, err)
			assertAccountInstructionsNotExposed(t, c, recorder, err, suffix)
			var fallbackErr *openAIWSFallbackError
			require.Equal(t, tt.wantFBErr, errors.As(err, &fallbackErr))
			_, hasOpsErr := c.Get(OpsUpstreamErrorMessageKey)
			require.Equal(t, tt.wantOpsErr, hasOpsErr)
		})
	}
	require.False(t, sink.ContainsMessage(suffix), "structured logs must not expose account instructions")
}

func forwardAccountInstructionsWSReadErrorForTest(
	t *testing.T,
	suffix string,
	events [][]byte,
) (*httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"client instructions","input":"hello"}`)
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

	conn := &accountInstructionsWSReadErrorConn{
		openAIWSCaptureConn: openAIWSCaptureConn{events: events},
		readErr: coderws.CloseError{
			Code:   coderws.StatusPolicyViolation,
			Reason: "echoed account instructions: " + suffix,
		},
	}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&accountInstructionsWSErrorDialer{conn: conn})
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

	_, err := svc.Forward(context.Background(), c, account, body)
	return recorder, c, err
}

type accountInstructionsWSReadErrorConn struct {
	openAIWSCaptureConn
	readErr error
}

func (c *accountInstructionsWSReadErrorConn) ReadMessage(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	hasEvents := len(c.events) > 0
	c.mu.Unlock()
	if hasEvents {
		return c.openAIWSCaptureConn.ReadMessage(ctx)
	}
	return nil, c.readErr
}

type accountInstructionsWSErrorDialer struct {
	conn openAIWSClientConn
}

func (d *accountInstructionsWSErrorDialer) Dial(
	context.Context,
	string,
	http.Header,
	string,
) (openAIWSClientConn, int, http.Header, error) {
	return d.conn, http.StatusSwitchingProtocols, nil, nil
}

type accountInstructionsWSStageErrorConn struct {
	mu       sync.Mutex
	writeErr error
	readErr  error
	events   [][]byte
	closed   bool
}

func (c *accountInstructionsWSStageErrorConn) WriteJSON(context.Context, any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errOpenAIWSConnClosed
	}
	return c.writeErr
}

func (c *accountInstructionsWSStageErrorConn) ReadMessage(context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errOpenAIWSConnClosed
	}
	if len(c.events) > 0 {
		event := append([]byte(nil), c.events[0]...)
		c.events = c.events[1:]
		return event, nil
	}
	if c.readErr != nil {
		return nil, c.readErr
	}
	return nil, io.EOF
}

func (c *accountInstructionsWSStageErrorConn) Ping(context.Context) error { return nil }

func (c *accountInstructionsWSStageErrorConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func accountInstructionsWebSocketErrorTestConfig(prewarm bool) *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = prewarm
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	return cfg
}

func accountInstructionsForwardWSMainWriteForTest(
	t *testing.T,
	svc *OpenAIGatewayService,
	cfg *config.Config,
	account *Account,
	conn openAIWSClientConn,
	payload map[string]any,
) error {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&accountInstructionsWSErrorDialer{conn: conn})
	svc.openaiWSPool = pool
	agentTaskRecoveryTried := false
	_, err := svc.forwardOpenAIWSV2(
		context.Background(),
		c,
		account,
		payload,
		"sk-test",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		false,
		false,
		"gpt-5.4",
		"gpt-5.4",
		time.Now(),
		0,
		"",
		&agentTaskRecoveryTried,
	)
	return err
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
