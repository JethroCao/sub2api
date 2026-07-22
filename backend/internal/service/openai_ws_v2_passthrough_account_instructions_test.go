package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func dialPassthroughAccountInstructionsClient(t *testing.T, serverURL string, firstPayload string) *coderws.Conn {
	t.Helper()
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(serverURL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(firstPayload))
	cancelWrite()
	require.NoError(t, err)
	return clientConn
}

func completePassthroughAccountInstructionsTurn(
	t *testing.T,
	clientConn *coderws.Conn,
	upstream *stagedPassthroughConn,
	responseID string,
) {
	t.Helper()
	upstream.Send(`{"type":"response.completed","response":{"id":"` + responseID + `","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	event, err := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, responseID, gjson.GetBytes(event, "response.id").String())
}

func TestOpenAIWSPassthroughAccountCustomInstructionsEveryCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)

	const suffix = "passthrough account suffix"
	account := passthroughLifecycleAccount()
	account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	clientConn := dialPassthroughAccountInstructionsClient(
		t,
		server.URL,
		`{"type":"response.create","model":"gpt-5.1","instructions":"  first client instructions  "}`,
	)
	defer func() { _ = clientConn.CloseNow() }()

	first := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "  first client instructions  \n\n"+suffix, gjson.GetBytes(first, "instructions").String())
	require.Equal(t, 1, strings.Count(gjson.GetBytes(first, "instructions").String(), suffix))
	completePassthroughAccountInstructionsTurn(t, clientConn, upstream, "resp_passthrough_instructions_1")

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_passthrough_instructions_1","instructions":"\tsecond client instructions\n"}`)))
	cancelWrite()
	second := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "\tsecond client instructions\n\n\n"+suffix, gjson.GetBytes(second, "instructions").String())
	require.Equal(t, 1, strings.Count(gjson.GetBytes(second, "instructions").String(), suffix))
	completePassthroughAccountInstructionsTurn(t, clientConn, upstream, "resp_passthrough_instructions_2")

	writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_passthrough_instructions_2","instructions":"\tsecond client instructions\n\n\npassthrough account suffix"}`)))
	cancelWrite()
	replay := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "\tsecond client instructions\n\n\n"+suffix, gjson.GetBytes(replay, "instructions").String())
	require.Equal(t, 1, strings.Count(gjson.GetBytes(replay, "instructions").String(), suffix))
	completePassthroughAccountInstructionsTurn(t, clientConn, upstream, "resp_passthrough_instructions_3")

	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough account-instructions server did not exit")
	}
}

func TestOpenAIWSPassthroughAccountCustomInstructionsNoopWhenUnconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		passthroughLifecycleAccount(),
	)
	defer server.Close()

	clientConn := dialPassthroughAccountInstructionsClient(
		t,
		server.URL,
		`{"type":"response.create","model":"gpt-5.1","instructions":"  unchanged client instructions  "}`,
	)
	defer func() { _ = clientConn.CloseNow() }()

	first := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "  unchanged client instructions  ", gjson.GetBytes(first, "instructions").String())
	completePassthroughAccountInstructionsTurn(t, clientConn, upstream, "resp_passthrough_noop")
	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough no-op server did not exit")
	}
}

func TestOpenAIWSPassthroughAccountCustomInstructionsRejectsNonString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	account := passthroughLifecycleAccount()
	account.Credentials[OpenAICustomInstructionsCredentialKey] = "private passthrough suffix"
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	clientConn := dialPassthroughAccountInstructionsClient(
		t,
		server.URL,
		`{"type":"response.create","model":"gpt-5.1","instructions":{"invalid":true}}`,
	)
	defer func() { _ = clientConn.CloseNow() }()

	select {
	case err := <-serverErr:
		var closeErr *OpenAIWSClientCloseError
		require.ErrorAs(t, err, &closeErr)
		require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
		require.Equal(t, "invalid websocket instructions", closeErr.Reason())
		require.NotContains(t, closeErr.Error(), "private passthrough suffix")
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough invalid-instructions server did not exit")
	}
	select {
	case payload := <-upstream.writes:
		t.Fatalf("invalid instructions reached upstream: %s", payload)
	default:
	}
}

func TestOpenAIWSPassthroughAccountCustomInstructionsRedactsFailureEvents(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		payload   string
	}{
		{
			name:      "bare error",
			eventType: "error",
			payload:   `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"echoed private \u4e2d\n\" suffix"}}`,
		},
		{
			name:      "response failed",
			eventType: "response.failed",
			payload:   `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"type":"invalid_request_error","code":"invalid_request","message":"echoed private \u4e2d\n\" suffix"}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			controlCtx, cancelControl := context.WithCancelCause(context.Background())
			defer cancelControl(context.Canceled)
			account := passthroughLifecycleAccount()
			const suffix = "private 中\n\" suffix"
			account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
			upstream := newStagedPassthroughConn()
			server, serverErr := startPassthroughLifecycleServer(
				t,
				controlCtx,
				newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
				account,
			)
			defer server.Close()

			clientConn := dialPassthroughAccountInstructionsClient(t, server.URL, `{"type":"response.create","model":"gpt-5.1"}`)
			defer func() { _ = clientConn.CloseNow() }()
			_ = requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
			upstream.Send(tt.payload)

			got, err := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
			require.NoError(t, err)
			require.Equal(t, tt.eventType, gjson.GetBytes(got, "type").String())
			require.NotContains(t, string(got), suffix)
			require.NotContains(t, string(got), `private \u4e2d\n\" suffix`)
			require.Contains(t, string(got), openAIAccountInstructionsRedaction)

			require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
			select {
			case err := <-serverErr:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("passthrough failure-event server did not exit")
			}
		})
	}
}

func TestOpenAIWSPassthroughAccountCustomInstructionsRedactsRateLimitDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	account := passthroughLifecycleAccount()
	const suffix = "rate private 中\n suffix"
	account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})

	clientConn := dialPassthroughAccountInstructionsClient(t, server.URL, `{"type":"response.create","model":"gpt-5.1"}`)
	defer func() { _ = clientConn.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	upstream.Send(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"echoed rate private \u4e2d\n suffix"}}`)

	var serverResult error
	select {
	case serverResult = <-serverErr:
		require.Error(t, serverResult)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough rate-limit server did not exit")
	}
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, serverResult, &failoverErr)
	require.NotContains(t, string(failoverErr.ResponseBody), suffix)
	require.NotContains(t, string(failoverErr.ResponseBody), `rate private \u4e2d\n suffix`)
	require.Contains(t, string(failoverErr.ResponseBody), openAIAccountInstructionsRedaction)
	require.NotContains(t, logs.String(), suffix)
	require.NotContains(t, logs.String(), `rate private \u4e2d\n suffix`)
}

func TestOpenAIWSPassthroughAccountCustomInstructionsRedactsTransportErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	account := passthroughLifecycleAccount()
	const suffix = "transport private 中\n suffix"
	account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})

	clientConn := dialPassthroughAccountInstructionsClient(t, server.URL, `{"type":"response.create","model":"gpt-5.1"}`)
	defer func() { _ = clientConn.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	upstream.SendReadError(errors.New(`upstream read failed: transport private \u4e2d\n suffix`))
	_, clientReadErr := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.Error(t, clientReadErr)
	require.NotContains(t, clientReadErr.Error(), suffix)
	require.NotContains(t, clientReadErr.Error(), `transport private \u4e2d\n suffix`)

	select {
	case err := <-serverErr:
		require.Error(t, err)
		require.NotContains(t, err.Error(), suffix)
		require.NotContains(t, err.Error(), `transport private \u4e2d\n suffix`)
		require.Contains(t, err.Error(), openAIAccountInstructionsRedaction)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough transport-error server did not exit")
	}
	require.NotContains(t, logs.String(), suffix)
	require.NotContains(t, logs.String(), `transport private \u4e2d\n suffix`)
}

func TestOpenAIWSPassthroughAccountCustomInstructionsRedactsNonResponseWriteErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	account := passthroughLifecycleAccount()
	const suffix = "write private 中\n suffix"
	const escapedSuffix = `write private \u4e2d\n suffix`
	account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})

	clientConn := dialPassthroughAccountInstructionsClient(t, server.URL, `{"type":"response.create","model":"gpt-5.1"}`)
	defer func() { _ = clientConn.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)

	// The relay starts reading follow-up client frames after the first
	// downstream frame. A successful non-response.create frame must remain an
	// exact passthrough before the next write exercises the failure path.
	upstream.Send(`{"type":"response.created","response":{"id":"resp_write_error","model":"gpt-5.1"}}`)
	_, err := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.NoError(t, err)
	const successfulFrame = `{"type":"session.update","session":{"model":"gpt-5.1"}}`
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(successfulFrame)))
	cancelWrite()
	require.Equal(t, successfulFrame, string(requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)))

	upstream.SendWriteError(errors.New("upstream write failed: " + suffix + "; escaped: " + escapedSuffix))
	writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"conversation.item.create","item":{"type":"message"}}`)))
	cancelWrite()

	_, clientReadErr := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.Error(t, clientReadErr)
	require.NotContains(t, clientReadErr.Error(), suffix)
	require.NotContains(t, clientReadErr.Error(), escapedSuffix)

	select {
	case err := <-serverErr:
		require.Error(t, err)
		require.NotContains(t, err.Error(), suffix)
		require.NotContains(t, err.Error(), escapedSuffix)
		require.Contains(t, err.Error(), openAIAccountInstructionsRedaction)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough non-response write-error server did not exit")
	}
	require.Contains(t, logs.String(), "relay_trace")
	require.Contains(t, logs.String(), "read_client_fail")
	require.Contains(t, logs.String(), openAIAccountInstructionsRedaction)
	require.NotContains(t, logs.String(), suffix)
	require.NotContains(t, logs.String(), escapedSuffix)
}

func TestOpenAIWSPassthroughAccountCustomInstructionsRedactsUpstreamCloseErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	account := passthroughLifecycleAccount()
	const suffix = "close private 中 suffix"
	account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})

	clientConn := dialPassthroughAccountInstructionsClient(t, server.URL, `{"type":"response.create","model":"gpt-5.1"}`)
	defer func() { _ = clientConn.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	upstream.SendReadError(coderws.CloseError{Code: coderws.StatusInternalError, Reason: "echoed " + suffix})
	_, clientReadErr := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.Error(t, clientReadErr)
	require.NotContains(t, clientReadErr.Error(), suffix)

	select {
	case err := <-serverErr:
		require.Error(t, err)
		require.NotContains(t, err.Error(), suffix)
		require.Contains(t, err.Error(), openAIAccountInstructionsRedaction)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough upstream-close server did not exit")
	}
	require.NotContains(t, logs.String(), suffix)
}

func TestOpenAIWSPassthroughAccountCustomInstructionsPreservesSuccessfulEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	account := passthroughLifecycleAccount()
	const suffix = "success private suffix"
	account.Credentials[OpenAICustomInstructionsCredentialKey] = suffix
	upstream := newStagedPassthroughConn()
	server, serverErr := startPassthroughLifecycleServer(
		t,
		controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		account,
	)
	defer server.Close()

	clientConn := dialPassthroughAccountInstructionsClient(t, server.URL, `{"type":"response.create","model":"gpt-5.1"}`)
	defer func() { _ = clientConn.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	success := `{"type":"response.completed","response":{"id":"resp_success","model":"gpt-5.1","output":[{"type":"message","content":[{"type":"output_text","text":"success private suffix"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`
	upstream.Send(success)

	got, err := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, success, string(got))
	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough success server did not exit")
	}
}
