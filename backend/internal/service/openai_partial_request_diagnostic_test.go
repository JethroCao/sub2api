package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestIsOpenAIPartialMissingError(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		upstreamMsg  string
		upstreamBody []byte
		want         bool
	}{
		{
			name:         "structured volcano error",
			statusCode:   http.StatusBadRequest,
			upstreamBody: []byte(`{"error":{"code":"MissingParameter","message":"The request failed because it is missing ` + "`partial`" + ` parameter.","param":"partial","type":"BadRequest"}}`),
			want:         true,
		},
		{
			name:        "message fallback",
			statusCode:  http.StatusBadRequest,
			upstreamMsg: "The partial parameter is required",
			want:        true,
		},
		{
			name:         "wrong status",
			statusCode:   http.StatusBadGateway,
			upstreamBody: []byte(`{"error":{"code":"MissingParameter","param":"partial"}}`),
			want:         false,
		},
		{
			name:         "different missing parameter",
			statusCode:   http.StatusBadRequest,
			upstreamBody: []byte(`{"error":{"code":"MissingParameter","message":"model is required","param":"model"}}`),
			want:         false,
		},
		{
			name:         "partial mentioned but not missing",
			statusCode:   http.StatusBadRequest,
			upstreamBody: []byte(`{"error":{"code":"InvalidParameter","message":"partial must be a boolean","param":"partial"}}`),
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isOpenAIPartialMissingError(tt.statusCode, tt.upstreamMsg, tt.upstreamBody))
		})
	}
}

func TestLogOpenAIPartialMissingRequestBodiesLogsClientAndUpstreamBodies(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	clientBody := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"assistant","content":"hello"}]}`)
	upstreamBody := []byte(`{"model":"ep-test","input":[{"role":"assistant","content":"hello","partial":true}]}`)
	errorBody := []byte(`{"error":{"code":"MissingParameter","message":"The request failed because it is missing ` + "`partial`" + ` parameter.","param":"partial"}}`)
	c := &gin.Context{}
	c.Set("api_key", &APIKey{ID: 8})

	logOpenAIPartialMissingRequestBodies(
		ctx,
		c,
		&Account{ID: 12, Name: "火山引擎deepseek"},
		http.StatusBadRequest,
		"The request failed because it is missing `partial` parameter.",
		errorBody,
		clientBody,
		upstreamBody,
		"gpt-5.6-sol",
		"ep-test",
	)

	entries := observed.All()
	require.Len(t, entries, 2)

	clientFields := entries[0].ContextMap()
	require.Equal(t, "openai.partial_missing_request_body", entries[0].Message)
	require.Equal(t, "client", clientFields["body_kind"])
	require.Equal(t, string(clientBody), clientFields["request_body"])
	require.EqualValues(t, len(clientBody), clientFields["request_body_bytes"])
	require.NotEmpty(t, clientFields["request_body_sha256"])
	require.EqualValues(t, 8, clientFields["api_key_id"])

	upstreamFields := entries[1].ContextMap()
	require.Equal(t, "upstream", upstreamFields["body_kind"])
	require.Equal(t, string(upstreamBody), upstreamFields["request_body"])
	require.Equal(t, "MissingParameter", upstreamFields["upstream_error_code"])
	require.Equal(t, "partial", upstreamFields["upstream_error_param"])
}

func TestLogOpenAIPartialMissingRequestBodiesIgnoresOtherErrors(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))

	logOpenAIPartialMissingRequestBodies(
		ctx,
		nil,
		&Account{ID: 12},
		http.StatusBadRequest,
		"Unknown parameter: namespace",
		[]byte(`{"error":{"code":"unknown_parameter","param":"namespace"}}`),
		[]byte(`{"client":true}`),
		[]byte(`{"upstream":true}`),
		"gpt-5.6-sol",
		"ep-test",
	)

	require.Empty(t, observed.All())
}

func TestOpenAIGatewayForwardLogsEntryAndFailedUpstreamBodiesForPartialMissing(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	clientBody := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"assistant","content":"draft","partial":false}]}`)
	errorBody := `{"error":{"code":"MissingParameter","message":"The request failed because it is missing ` + "`partial`" + ` parameter.","param":"partial","type":"BadRequest"}}`
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, errorBody)}
	account := newOpenAIRejectedFieldTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.6-sol": "ep-test"}
	account.Extra[OpenAIResponsesMessagePartialCompatExtraKey] = true
	c := newOpenAIRejectedFieldTestContext(clientBody)
	c.Set("api_key", &APIKey{ID: 8})

	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, account, clientBody)

	require.Error(t, err)
	require.Nil(t, result)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "ep-test", gjson.GetBytes(upstream.bodies[0], "model").String())
	require.True(t, gjson.GetBytes(upstream.bodies[0], "input.0.partial").Bool())

	entries := observed.FilterMessage(openAIPartialMissingRequestBodyLogMessage).All()
	require.Len(t, entries, 2)
	require.Equal(t, "client", entries[0].ContextMap()["body_kind"])
	require.Equal(t, string(clientBody), entries[0].ContextMap()["request_body"])
	require.Equal(t, "upstream", entries[1].ContextMap()["body_kind"])
	require.Equal(t, string(upstream.bodies[0]), entries[1].ContextMap()["request_body"])
}
