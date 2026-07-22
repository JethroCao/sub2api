package service

import (
	"context"
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
