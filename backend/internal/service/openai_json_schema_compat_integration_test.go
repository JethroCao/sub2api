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
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesJSONSchemaCompatibilityNative(t *testing.T) {
	body := []byte(`{
		"model":"alias","input":"return person","stream":false,
		"text":{"format":{"type":"json_schema","name":"person","strict":true,"schema":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}}
	}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":2}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: jsonSchemaCompatTestConfig(), httpUpstream: upstream}
	account := jsonSchemaCompatTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"alias": "deepseek-v4-pro-260425"}
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesSupported: true,
		OpenAIJSONSchemaModeExtraKey:             OpenAIJSONSchemaModeForceJSONObject,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "deepseek-v4-pro-260425", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "json_object", gjson.GetBytes(upstream.lastBody, "text.format.type").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "instructions").String(), `"required":["name"]`)
}

func TestForwardRawChatJSONSchemaCompatibility(t *testing.T) {
	body := []byte(`{
		"model":"alias","messages":[{"role":"user","content":"return person"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"person","strict":true,"schema":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}}
	}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl","object":"chat.completion","model":"deepseek-v4-pro-260425","choices":[{"index":0,"message":{"role":"assistant","content":"{\"name\":\"Ada\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: jsonSchemaCompatTestConfig(), httpUpstream: upstream}
	account := jsonSchemaCompatTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"alias": "deepseek-v4-pro-260425"}
	account.Extra = map[string]any{OpenAIJSONSchemaModeExtraKey: OpenAIJSONSchemaModeForceJSONObject}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "deepseek-v4-pro-260425", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "json_object", gjson.GetBytes(upstream.lastBody, "response_format.type").String())
	require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "messages.#").Int())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "messages.0.content").String(), `"required":["name"]`)
}

func TestForwardResponsesJSONSchemaCompatibilityChatFallback(t *testing.T) {
	body := []byte(`{
		"model":"alias","input":"return person","stream":false,
		"text":{"format":{"type":"json_schema","name":"person","strict":true,"schema":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}}
	}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl","object":"chat.completion","model":"deepseek-v4-pro-260425","choices":[{"index":0,"message":{"role":"assistant","content":"{\"name\":\"Ada\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: jsonSchemaCompatTestConfig(), httpUpstream: upstream}
	account := jsonSchemaCompatTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"alias": "deepseek-v4-pro-260425"}
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
		OpenAIJSONSchemaModeExtraKey:        OpenAIJSONSchemaModeForceJSONObject,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "deepseek-v4-pro-260425", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "json_object", gjson.GetBytes(upstream.lastBody, "response_format.type").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "messages.0.content").String(), `"required":["name"]`)
}

func TestForwardChatJSONSchemaCompatibilityResponsesBridge(t *testing.T) {
	body := []byte(`{
		"model":"alias","messages":[{"role":"user","content":"return person"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"person","strict":true,"schema":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}}
	}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop after capture"}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 9, Name: "compat", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "model_mapping": map[string]any{"alias": "deepseek-v4-pro-260425"}},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: true,
			OpenAIJSONSchemaModeExtraKey:             OpenAIJSONSchemaModeForceJSONObject,
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, "deepseek-v4-pro-260425", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "json_object", gjson.GetBytes(upstream.lastBody, "text.format.type").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "instructions").String(), `"required":["name"]`)
}

func jsonSchemaCompatTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return cfg
}

func jsonSchemaCompatTestAccount() *Account {
	return &Account{
		ID:          8,
		Name:        "json-schema-compatible",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example/v1",
		},
	}
}
