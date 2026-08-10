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

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesMessagePartialCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		passthrough bool
		wantModel   string
	}{
		{name: "native forwarding", wantModel: "deepseek-v4-pro-260425"},
		{name: "passthrough forwarding", passthrough: true, wantModel: "alias"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{
				"model":"alias",
				"input":[
					{"type":"message","role":"user","content":"hello"},
					{"type":"reasoning","encrypted_content":"cipher"},
					{"type":"message","role":"assistant","content":[{"type":"output_text","text":"draft"}],"partial":true}
				],
				"stream":false
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
				openai_compat.ExtraKeyResponsesSupported:    true,
				OpenAIResponsesMessagePartialCompatExtraKey: true,
			}
			if tt.passthrough {
				account.Extra["openai_passthrough"] = true
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.wantModel, gjson.GetBytes(upstream.lastBody, "model").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.0.partial").Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.1.partial").Exists())
			require.True(t, gjson.GetBytes(upstream.lastBody, "input.2.partial").Bool())
		})
	}
}

func TestForwardResponsesRetriesPartialMissingAfterToolSuffix(t *testing.T) {
	body := []byte(`{
		"model":"alias",
		"stream":false,
		"input":[
			{"type":"message","role":"assistant","content":"draft"},
			{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"{}"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}
		]
	}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(
			http.StatusBadRequest,
			`{"error":{"code":"MissingParameter","message":"The request failed because it is missing `+"`partial`"+` parameter.","param":"partial","type":"BadRequest"}}`,
		),
		newOpenAIRejectedFieldTestResponse(
			http.StatusOK,
			`{"id":"resp_retry_ok","output":[],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}`,
		),
	}}
	svc := &OpenAIGatewayService{cfg: jsonSchemaCompatTestConfig(), httpUpstream: upstream}
	account := jsonSchemaCompatTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"alias": "deepseek-v4-pro-260425"}
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesSupported:    true,
		OpenAIResponsesMessagePartialCompatExtraKey: true,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.False(t, gjson.GetBytes(upstream.bodies[0], "input.0.partial").Exists())
	require.True(t, gjson.GetBytes(upstream.bodies[1], "input.0.partial").Bool())
	for _, index := range []int{1, 2} {
		require.False(t, gjson.GetBytes(upstream.bodies[1], fmt.Sprintf("input.%d.partial", index)).Exists())
	}
}
