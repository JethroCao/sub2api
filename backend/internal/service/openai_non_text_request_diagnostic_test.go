package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestIsOpenAINonTextInputUnsupportedError(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		upstreamMsg  string
		upstreamBody []byte
		want         bool
	}{
		{
			name:        "only text input",
			statusCode:  http.StatusBadRequest,
			upstreamMsg: "Model only support text input Request id: req-1",
			want:        true,
		},
		{
			name:         "does not support image input from body",
			statusCode:   http.StatusBadRequest,
			upstreamBody: []byte(`{"error":{"code":"InvalidParameter","message":"Model do not support image input Request id: req-2","type":"BadRequest"}}`),
			want:         true,
		},
		{
			name:        "case insensitive",
			statusCode:  http.StatusBadRequest,
			upstreamMsg: "MODEL ONLY SUPPORT TEXT INPUT",
			want:        true,
		},
		{
			name:        "wrong status",
			statusCode:  http.StatusBadGateway,
			upstreamMsg: "Model only support text input",
			want:        false,
		},
		{
			name:        "different invalid parameter",
			statusCode:  http.StatusBadRequest,
			upstreamMsg: "Unknown parameter: input[26].namespace",
			want:        false,
		},
		{
			name:        "generic image mention",
			statusCode:  http.StatusBadRequest,
			upstreamMsg: "invalid image URL",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isOpenAINonTextInputUnsupportedError(tt.statusCode, tt.upstreamMsg, tt.upstreamBody))
		})
	}
}

func TestLogOpenAINonTextInputRequestBodiesLogsClientAndUpstreamBodies(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	clientBody := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]}`)
	upstreamBody := []byte(`{"model":"ep-test","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]}`)
	errorBody := []byte(`{"error":{"code":"InvalidParameter","message":"Model only support text input Request id: req-1","param":"","type":"BadRequest"}}`)
	c := &gin.Context{}
	c.Set("api_key", &APIKey{ID: 125})

	logOpenAINonTextInputRequestBodies(
		ctx,
		c,
		&Account{ID: 12, Name: "火山引擎deepseek"},
		http.StatusBadRequest,
		"Model only support text input Request id: req-1",
		errorBody,
		clientBody,
		upstreamBody,
		"gpt-5.5",
		"ep-test",
	)

	entries := observed.FilterMessage(openAINonTextInputRequestBodyLogMessage).All()
	require.Len(t, entries, 2)

	clientFields := entries[0].ContextMap()
	require.Equal(t, "client", clientFields["body_kind"])
	require.Equal(t, string(clientBody), clientFields["request_body"])
	require.EqualValues(t, len(clientBody), clientFields["request_body_bytes"])
	require.NotEmpty(t, clientFields["request_body_sha256"])
	require.EqualValues(t, 12, clientFields["account_id"])
	require.EqualValues(t, 125, clientFields["api_key_id"])
	require.Equal(t, "InvalidParameter", clientFields["upstream_error_code"])
	require.Equal(t, "gpt-5.5", clientFields["original_model"])
	require.Equal(t, "ep-test", clientFields["upstream_model"])

	upstreamFields := entries[1].ContextMap()
	require.Equal(t, "upstream", upstreamFields["body_kind"])
	require.Equal(t, string(upstreamBody), upstreamFields["request_body"])
}

func TestLogOpenAINonTextInputRequestBodiesIgnoresOtherErrors(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))

	logOpenAINonTextInputRequestBodies(
		ctx,
		nil,
		&Account{ID: 12},
		http.StatusBadRequest,
		"Unknown parameter: namespace",
		[]byte(`{"error":{"code":"unknown_parameter","param":"namespace"}}`),
		[]byte(`{"client":true}`),
		[]byte(`{"upstream":true}`),
		"gpt-5.5",
		"ep-test",
	)

	require.Empty(t, observed.All())
}

func TestOpenAIGatewayForwardLogsClientAndFailedUpstreamBodiesForNonTextInputError(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	clientBody := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]}`)
	errorBody := `{"error":{"code":"InvalidParameter","message":"Model only support text input Request id: req-1","param":"","type":"BadRequest"}}`
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, errorBody)}
	account := newOpenAIRejectedFieldTestAccount()
	account.ID = 12
	account.Name = "火山引擎deepseek"
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.5": "ep-test"}
	c := newOpenAIRejectedFieldTestContext(clientBody)
	c.Set("api_key", &APIKey{ID: 125})

	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, account, clientBody)

	require.Error(t, err)
	require.Nil(t, result)
	require.Len(t, upstream.bodies, 1)

	entries := observed.FilterMessage(openAINonTextInputRequestBodyLogMessage).All()
	require.Len(t, entries, 2)
	require.Equal(t, "client", entries[0].ContextMap()["body_kind"])
	require.Equal(t, string(clientBody), entries[0].ContextMap()["request_body"])
	require.Equal(t, "upstream", entries[1].ContextMap()["body_kind"])
	require.Equal(t, string(upstream.bodies[0]), entries[1].ContextMap()["request_body"])
}
