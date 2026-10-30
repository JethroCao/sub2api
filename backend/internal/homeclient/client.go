package homeclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Client struct {
	cfg             Config
	tls             *tls.Config
	instanceID      string
	mu              sync.Mutex
	running, closed bool
	current         *generation
	stop, done      chan struct{}
	closeErr        error
}
type generation struct {
	releaseMu         sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	work              context.Context
	cancelWork        context.CancelFunc
	policy            lifecycle
	conns             map[net.Conn]struct{}
	active            map[*Lease]struct{}
	pending           int
	accepting, failed bool
	ledger            map[releaseKey]*releaseCount
	wake, changed     chan struct{}
	usage             chan usagePayload
	workers           sync.WaitGroup
}

func New(cfg Config) (*Client, error) {
	tc, e := tlsConfig(cfg)
	if e != nil {
		return nil, e
	}
	return &Client{cfg: cfg, tls: tc, instanceID: uuid.NewString(), stop: make(chan struct{}), done: make(chan struct{})}, nil
}
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.current != nil && c.current.accepting && !c.current.failed
}

// Run supervises membership until ctx is cancelled or Close is called. Run
// blocks; start it in a goroutine. A client may only be run once.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.running {
		c.mu.Unlock()
		return errors.New("home: client already running")
	}
	c.running = true
	c.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClose := context.AfterFunc(runCtx, func() { c.requestClose() })
	defer stopClose()
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-runCtx.Done():
		}
	}()
	defer func() { c.mu.Lock(); c.closed = true; c.running = false; close(c.done); c.mu.Unlock() }()
	backoff := 50 * time.Millisecond
	for {
		if runCtx.Err() != nil {
			return ctx.Err()
		}
		g, e := c.establish(runCtx)
		if e != nil {
			if !pause(runCtx, backoff+time.Duration(rand.Int64N(int64(backoff/2)+1))) {
				return ctx.Err()
			}
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 50 * time.Millisecond
		c.mu.Lock()
		c.current = g
		if !c.closed && !g.failed {
			g.accepting = true
		}
		closing := c.closed
		c.mu.Unlock()
		if closing {
			c.shutdown(g)
			return ctx.Err()
		}
		select {
		case <-runCtx.Done():
			c.shutdown(g)
			return ctx.Err()
		case <-g.ctx.Done():
		}
		// A cancelled upstream is not necessarily stopped. Do not obtain a new
		// membership until every old lease has explicitly finished.
		if !c.waitDrained(runCtx, g) {
			c.shutdown(g)
			return ctx.Err()
		}
		g.workers.Wait()
		c.mu.Lock()
		if c.current == g {
			c.current = nil
		}
		c.mu.Unlock()
	}
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (c *Client) requestClose() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.stop)
	}
}
func (c *Client) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
		if !c.running {
			close(c.done)
		}
	}
	done := c.done
	c.mu.Unlock()
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}
func (c *Client) establish(ctx context.Context) (*generation, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	boot, e := c.dial(ctx)
	if e != nil {
		return nil, e
	}
	defer boot.Close()
	stopBoot := context.AfterFunc(ctx, func() { boot.Close() })
	defer stopBoot()
	d, _ := ctx.Deadline()
	boot.SetDeadline(d)
	if e = writeCommand(boot, "GET", "config"); e != nil {
		return nil, e
	}
	f, e := readFrame(bufio.NewReader(boot))
	if e != nil || f.kind != '$' {
		return nil, errors.New("home: config unavailable")
	}
	p, e := parseLifecycle(f.text)
	if e != nil {
		return nil, e
	}
	sub, e := c.dial(ctx)
	if e != nil {
		return nil, e
	}
	stopSub := context.AfterFunc(ctx, func() { sub.Close() })
	defer stopSub()
	sub.SetDeadline(d)
	fail := true
	defer func() {
		if fail {
			sub.Close()
		}
	}()
	if e = writeCommand(sub, "SUBSCRIBE", "config", strconv.FormatInt(p.Revision, 10), c.instanceID); e != nil {
		return nil, e
	}
	reader := bufio.NewReader(sub)
	f, e = readFrame(reader)
	if e != nil || f.kind != '*' || len(f.array) != 3 || f.array[0].text != "subscribe" || f.array[1].text != "config" || f.array[2].kind != ':' || f.array[2].number != 1 {
		return nil, errors.New("home: membership ACK unavailable")
	}
	// Bootstrap sockets carry immutable pre-membership identity; discard them.
	boot.Close()
	sub.SetDeadline(time.Time{})
	stopSub()
	gctx, gcancel := context.WithCancel(context.Background())
	work, cancelWork := context.WithCancel(gctx)
	g := &generation{ctx: gctx, cancel: gcancel, work: work, cancelWork: cancelWork, policy: p, conns: map[net.Conn]struct{}{sub: {}}, active: map[*Lease]struct{}{}, ledger: map[releaseKey]*releaseCount{}, wake: make(chan struct{}, 1), changed: make(chan struct{}, 1), usage: make(chan usagePayload, 256)}
	g.workers.Add(1)
	go func() { defer g.workers.Done(); c.readSubscription(g, sub, reader) }()
	f, _, e = c.exchange(ctx, g, commandTimeout, "PING")
	if e != nil || f.kind != '+' || f.text != "PONG" {
		c.fence(g)
		g.workers.Wait()
		return nil, errors.New("home: fresh membership probe failed")
	}
	g.workers.Add(2)
	go func() { defer g.workers.Done(); c.releaseLoop(g) }()
	go func() { defer g.workers.Done(); c.usageLoop(g) }()
	fail = false
	return g, nil
}
func (c *Client) readSubscription(g *generation, conn net.Conn, r *bufio.Reader) {
	defer c.fence(g)
	// Config parsing is off the reader so heartbeats cannot wait behind YAML.
	configs := make(chan string, 1)
	g.workers.Add(1)
	go func() {
		defer g.workers.Done()
		for {
			select {
			case <-g.ctx.Done():
				return
			case s := <-configs:
				p, e := parseLifecycle(s)
				if e != nil || p != g.policy {
					c.fence(g)
					return
				}
			}
		}
	}()
	for {
		conn.SetReadDeadline(time.Now().Add(g.policy.Heartbeat))
		f, e := readFrame(r)
		if e != nil || f.kind != '*' {
			return
		}
		a := f.array
		if len(a) == 2 && a[0].text == "pong" && a[1].text == "" {
			continue
		}
		if len(a) == 3 && a[0].text == "message" {
			switch a[1].text {
			case "config":
				select {
				case configs <- a[2].text:
				default:
					return
				}
				continue
			case "cluster":
				continue
			}
		}
		return // Includes unexpected resubscription ACKs.
	}
}
func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (c *Client) fence(g *generation) {
	c.mu.Lock()
	if g.failed {
		c.mu.Unlock()
		return
	}
	g.failed = true
	g.accepting = false
	g.cancelWork()
	for lease := range g.active {
		lease.cancel()
	}
	g.cancel()
	conns := make([]net.Conn, 0, len(g.conns))
	for conn := range g.conns {
		conns = append(conns, conn)
	}
	notify(g.changed)
	c.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}
func (c *Client) drained(g *generation) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return g.pending == 0 && len(g.active) == 0
}
func (c *Client) waitDrained(ctx context.Context, g *generation) bool {
	for {
		if c.drained(g) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-g.changed:
		}
	}
}
func (c *Client) shutdown(g *generation) {
	c.mu.Lock()
	g.accepting = false
	g.cancelWork()
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), g.policy.CancelBound)
	defer cancel()
	drained := c.waitDrained(ctx, g)
	if drained {
		for {
			c.mu.Lock()
			pending := false
			for _, v := range g.ledger {
				pending = pending || v.latest > v.acked
			}
			failed := g.failed
			c.mu.Unlock()
			if !pending || failed {
				break
			}
			notify(g.wake)
			select {
			case <-ctx.Done():
				drained = false
			case <-g.changed:
			}
			if !drained {
				break
			}
		}
	}
	if !drained {
		c.mu.Lock()
		c.closeErr = ErrDrainTimeout
		c.mu.Unlock()
	}
	c.fence(g)
	g.workers.Wait()
}
