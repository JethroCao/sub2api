package service

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

var errHomeWSIdentityChanged = errors.New("Home websocket credentials changed; reconnect required")

// A turn owns admission for one response.create, never the idle connection.
type homeWSTurn struct {
	lease    *homeclient.Lease
	home     *HomeCredentialsService
	account  *Account
	owner    *Account
	mu       sync.Mutex
	observer *homeResponseObserver
	stop     func()
	once     sync.Once
	done     chan struct{}
}

func (s *OpenAIGatewayService) beginHomeWSTurn(ctx context.Context, a *Account, model string) (*homeWSTurn, error) {
	owner, err := homeAttemptOwner(ctx, s.accountRepo, s.homeCredentials, a)
	if err != nil {
		return nil, homeControlError(err)
	}
	if !owner.HasHomeBinding() {
		return nil, nil
	}
	lease, err := beginHomeAttempt(ctx, s.homeCredentials, a, model)
	if err != nil {
		return nil, err
	}
	o := newHomeResponseObserver(200, http.Header{"Content-Type": []string{"text/event-stream"}})
	return &homeWSTurn{lease: lease, home: s.homeCredentials, account: a, owner: owner, observer: o, done: make(chan struct{})}, nil
}
func (t *homeWSTurn) bind(stop func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stop = stop
	t.mu.Unlock()
	go func() {
		select {
		case <-t.lease.Context().Done():
			t.finish()
		case <-t.done:
		}
	}()
}
func (t *homeWSTurn) observe(raw []byte) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observer.event(raw)
	return t.observer.terminal
}
func (t *homeWSTurn) finish() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		close(t.done)
		t.mu.Lock()
		result := t.observer.end(t.observer.terminal)
		stop := t.stop
		terminal := t.observer.terminal
		t.mu.Unlock()
		if (!terminal || result.Failed) && stop != nil {
			stop()
		}
		t.lease.Finish(result)
		recoverHomeAttempt(t.home, t.account, t.lease.Credential.AccessToken, result)
	})
}
func homeStopWSLease(lease *openAIWSConnLease) func() {
	return func() {
		if lease != nil && lease.conn != nil {
			lease.conn.abort()
			lease.MarkBroken()
		}
	}
}
func homeStopWSConn(conn openAIWSClientConn) func() {
	return func() {
		if force, ok := conn.(openAIWSForceCloser); ok {
			_ = force.CloseNow()
		} else {
			_ = conn.Close()
		}
	}
}

// Idle dials are credential-only; every subsequent model frame is separately
// admitted and compared with the identity that really authenticated this socket.
func (s *OpenAIGatewayService) homeWSHeaders(ctx context.Context, a *Account, headers http.Header) (http.Header, error) {
	owner, err := homeAttemptOwner(ctx, s.accountRepo, s.homeCredentials, a)
	if err != nil {
		return nil, homeControlError(err)
	}
	if !owner.HasHomeBinding() {
		return s.refreshOpenAIAgentIdentityHeaders(ctx, a, headers)
	}
	c, err := s.homeCredentials.Resolve(ctx, a)
	if err != nil {
		return nil, homeControlError(err)
	}
	headers = cloneHeader(headers)
	if headers == nil {
		headers = make(http.Header)
	}
	applyHomeHeaders(headers, c)
	return headers, nil
}

// The wrapper sees first and follow-up response.create frames alike. Observation
// is performed in the upstream read loop before another turn can be accepted;
// no delayed downstream callback can finish a later admission.
type homeWSFrameConn struct {
	inner      openaiwsv2.FrameConn
	svc        *OpenAIGatewayService
	account    *Account
	controlCtx context.Context
	identity   [32]byte
	stop       func()
	mu         sync.Mutex
	active     *homeWSTurn
	first      *homeWSTurn
}

func (c *homeWSFrameConn) WriteFrame(ctx context.Context, kind coderws.MessageType, raw []byte) error {
	if gjson.GetBytes(raw, "type").String() != "response.create" {
		return c.inner.WriteFrame(ctx, kind, raw)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		return errors.New("overlapping Home websocket turn")
	}
	turn := c.first
	c.first = nil
	var err error
	if turn == nil {
		turn, err = c.svc.beginHomeWSTurn(c.controlCtx, c.account, gjson.GetBytes(raw, "model").String())
		if err != nil {
			c.stop()
			return NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "shared upstream capacity unavailable; reconnect later", err)
		}
	}
	if turn != nil {
		if homeCredentialIdentity(turn.lease.Credential) != c.identity {
			c.stop()
			turn.finish()
			return NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "upstream credentials changed; reconnect conversation", errHomeWSIdentityChanged)
		}
		c.active = turn
		turn.bind(c.stop)
		if err := turn.lease.Context().Err(); err != nil {
			turn.finish()
			c.active = nil
			return err
		}
		// Preserve the per-write deadline while making membership loss cancel it.
		writeCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(turn.lease.Context(), cancel)
		err = c.inner.WriteFrame(writeCtx, kind, raw)
		stop()
		cancel()
	} else {
		err = c.inner.WriteFrame(ctx, kind, raw)
	}
	if err != nil && turn != nil {
		turn.finish()
		c.active = nil
	}
	return err
}
func (c *homeWSFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	kind, raw, err := c.inner.ReadFrame(ctx)
	c.mu.Lock()
	turn := c.active
	if turn != nil && (err != nil || turn.observe(raw)) {
		turn.finish()
		c.active = nil
	}
	c.mu.Unlock()
	return kind, raw, err
}
func (c *homeWSFrameConn) Close() error {
	c.stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		c.active.finish()
		c.active = nil
	}
	if c.first != nil {
		c.first.finish()
		c.first = nil
	}
	return nil
}
