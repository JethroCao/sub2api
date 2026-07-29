package service

import (
	"bytes"
	"context"
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

func TestForwardResponsesAssistantPrefillCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		passthrough bool
		wantModel   string
	}{
		{name: "native forwarding", wantModel: "ep-ark-glm"},
		{name: "passthrough forwarding", passthrough: true, wantModel: "alias"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{
				"model":"alias",
				"input":[
					{"type":"message","role":"user","content":"hello"},
					{"type":"message","role":"assistant","content":[{"type":"output_text"}]},
					{"type":"message","role":"assistant","content":"draft","partial":true}
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
			account.Credentials["model_mapping"] = map[string]any{"alias": "ep-ark-glm"}
			account.Extra = map[string]any{
				openai_compat.ExtraKeyResponsesSupported:      true,
				OpenAIResponsesMessagePartialCompatExtraKey:   true,
				OpenAIResponsesAssistantPrefillCompatExtraKey: true,
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
			require.Len(t, gjson.GetBytes(upstream.lastBody, "input").Array(), 3)
			require.Equal(t, "draft", gjson.GetBytes(upstream.lastBody, "input.1.content").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.1.partial").Exists())
			require.Equal(t, "user", gjson.GetBytes(upstream.lastBody, "input.2.role").String())
			require.Equal(t, "Continue.", gjson.GetBytes(upstream.lastBody, "input.2.content.0.text").String())
		})
	}
}
