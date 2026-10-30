package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// Force the read-side relay exit to win after the failed writer aborts the raw
// socket. The writer reports its error only once Relay observes the reader exit.
type homeReadFirstRelayConn struct {
	openaiwsv2.FrameConn
	readStarted, firstExit chan struct{}
	once                   sync.Once
}

func (c *homeReadFirstRelayConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	c.once.Do(func() { close(c.readStarted) })
	return c.FrameConn.ReadFrame(ctx)
}
func (c *homeReadFirstRelayConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	err := c.FrameConn.WriteFrame(ctx, kind, payload)
	select {
	case <-c.firstExit:
	case <-ctx.Done():
	}
	return err
}

type homeNextTurnClient struct {
	readStarted chan struct{}
	sent        bool
}

func (c *homeNextTurnClient) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	if c.sent {
		<-ctx.Done()
		return 0, nil, ctx.Err()
	}
	select {
	case <-c.readStarted:
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	c.sent = true
	return coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1-codex"}`), nil
}
func (*homeNextTurnClient) WriteFrame(context.Context, coderws.MessageType, []byte) error {
	return nil
}
func (*homeNextTurnClient) Close() error { return nil }

func TestHomeWSDenialRetainsControlErrorWhenReaderWins(t *testing.T) {
	for _, refusal := range []string{"capacity", "identity_changed"} {
		t.Run(refusal, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if refusal == "capacity" {
				for range 3 {
					lease, err := s.homeCredentials.Acquire(ctx, homeTransportAccount(), homeclient.Request{Model: transportHomeTestModel})
					require.NoError(t, err)
					defer lease.Finish(homeclient.Result{})
				}
			}
			var upstreamFrames atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					if _, _, err = conn.Read(ctx); err != nil {
						return
					}
					upstreamFrames.Add(1)
				}
			}))
			defer upstream.Close()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			frames := &homeWSFrameConn{inner: &openAIWSClientFrameConn{conn: conn}, svc: s, account: homeTransportAccount(), controlCtx: ctx, stop: func() { _ = conn.CloseNow() }}
			// The zero identity deliberately differs from the admitted credential
			// in the rotation case; capacity refusal occurs before this comparison.
			ordered := &homeReadFirstRelayConn{FrameConn: frames, readStarted: make(chan struct{}), firstExit: make(chan struct{})}
			client := &homeNextTurnClient{readStarted: ordered.readStarted}
			var closeCode coderws.StatusCode
			_, exit := openaiwsv2.Relay(ctx, client, ordered, []byte(`{"type":"response.create","model":"gpt-5.1-codex"}`), openaiwsv2.RelayOptions{
				FirstMessageSent: true,
				BeforeRelayCancel: func(exit openaiwsv2.RelayExit) {
					closeCode, _, _ = openAIWSPassthroughRelayClientClose(exit, 1)
					close(ordered.firstExit)
				},
			})
			require.NotNil(t, exit)
			require.Equal(t, "read_upstream", exit.Stage, "the ordering regression must exercise the reader winning")
			require.Equal(t, coderws.StatusTryAgainLater, closeCode, "the winning path must send the causal 1013 close")
			var closeErr *OpenAIWSClientCloseError
			require.ErrorAs(t, exit.Err, &closeErr)
			require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
			require.Zero(t, upstreamFrames.Load(), "denied or mismatched turn must never send a model frame")
			if refusal == "identity_changed" {
				transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
			}
		})
	}
}
