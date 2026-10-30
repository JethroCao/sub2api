package homeclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Lease owns one accounted Home admission. Credential is a value snapshot;
// callers must treat it as immutable. Finish must only be called once the
// upstream response/stream has actually stopped, including after cancellation.
// A cancelled Context alone does not release the admission.
type Lease struct {
	Credential      Credential
	client          *Client
	generation      *generation
	ctx             context.Context
	cancel          context.CancelFunc
	stop            func() bool
	once            sync.Once
	key             releaseKey
	credential      Credential
	provider, model string
	started         time.Time
}

func (l *Lease) Context() context.Context { return l.ctx }
func (l *Lease) Finish(result Result) {
	l.once.Do(func() {
		l.cancel()
		l.stop()
		c, g := l.client, l.generation
		c.mu.Lock()
		delete(g.active, l)
		if !g.failed {
			n := g.ledger[l.key]
			if n == nil {
				n = &releaseCount{}
				g.ledger[l.key] = n
			}
			n.latest++
		}
		failed := g.failed
		notify(g.changed)
		c.mu.Unlock()
		if !failed {
			notify(g.wake)
			select {
			case g.usage <- l.usage(result):
			default:
			}
		}
	})
}

type wireError struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	RetryAfterMS int64  `json:"retry_after_ms"`
}
type envelope struct {
	Error     *wireError `json:"error"`
	Model     string     `json:"model"`
	Provider  string     `json:"provider"`
	AuthIndex string     `json:"auth_index"`
	Auth      struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
		Metadata struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
			Email       string `json:"email"`
			Expired     string `json:"expired"`
		} `json:"metadata"`
	} `json:"auth"`
	Concurrency *struct {
		Accounted    bool   `json:"accounted"`
		CredentialID string `json:"credential_id"`
		Model        string `json:"model"`
	} `json:"concurrency"`
}

// Do not surface server-controlled messages or payload fragments: they may
// contain credentials. Types and retry hints are sufficient for callers.
func (e *wireError) safeError() *Error {
	kind := e.Type
	if len(kind) == 0 || len(kind) > 96 || strings.IndexFunc(kind, func(r rune) bool { return !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) >= 0 {
		kind = "remote_error"
	}
	delay := e.RetryAfterMS
	if delay < 0 {
		delay = 0
	}
	if delay > 60000 {
		delay = 60000
	}
	return &Error{Type: kind, Message: "Home refused the request", Retryable: e.Retryable, RetryAfter: time.Duration(delay) * time.Millisecond}
}
func (e *envelope) credential(id string) (Credential, error) {
	if e.Auth.ID != id || strings.TrimSpace(e.Auth.Metadata.AccessToken) == "" || e.Auth.Disabled || e.AuthIndex == "" {
		return Credential{}, errors.New("home: invalid credential identity or access token")
	}
	v := Credential{ID: e.Auth.ID, AuthIndex: e.AuthIndex, AccessToken: e.Auth.Metadata.AccessToken, AccountID: e.Auth.Metadata.AccountID, Email: e.Auth.Metadata.Email}
	if s := e.Auth.Metadata.Expired; s != "" {
		expiry, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return Credential{}, errors.New("home: invalid credential expiration")
		}
		if !expiry.After(time.Now()) {
			return Credential{}, errors.New("home: expired credential")
		}
		v.ExpiresAt = expiry
	}
	return v, nil
}
func validateID(id string) bool { u, e := uuid.Parse(id); return e == nil && u.String() == id }
func (c *Client) begin(ctx context.Context) (*generation, context.Context, func(), error) {
	if e := ctx.Err(); e != nil {
		return nil, nil, nil, e
	}
	c.mu.Lock()
	g := c.current
	if c.closed {
		c.mu.Unlock()
		return nil, nil, nil, ErrClosed
	}
	if g == nil || !g.accepting || g.failed {
		c.mu.Unlock()
		return nil, nil, nil, ErrNotReady
	}
	g.pending++
	c.mu.Unlock()
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.work, cancel)
	done := func() {
		stop()
		cancel()
		c.mu.Lock()
		g.pending--
		notify(g.changed)
		c.mu.Unlock()
	}
	return g, op, done, nil
}
func (c *Client) Acquire(ctx context.Context, req Request) (*Lease, error) {
	if !validateID(req.CredentialID) || strings.TrimSpace(req.Model) == "" || len(req.Model) > 1024 || len(req.SessionID) > 4096 {
		return nil, errors.New("home: valid credential UUID and model are required")
	}
	g, op, done, e := c.begin(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	payload, _ := json.Marshal(struct {
		Type     string            `json:"type"`
		Model    string            `json:"model"`
		Count    int               `json:"count"`
		Protocol int               `json:"concurrency_protocol"`
		Pinned   string            `json:"pinned_auth_id"`
		Excluded []string          `json:"excluded_auth_ids"`
		Headers  map[string]string `json:"headers"`
		Session  string            `json:"session_id,omitempty"`
	}{"auth", req.Model, 1, 1, req.CredentialID, []string{}, map[string]string{"authorization": "Bearer " + c.cfg.APIKey}, req.SessionID})
	f, issued, e := c.exchange(op, g, commandTimeout, "RPOP", string(payload))
	if e != nil {
		var refusal respError
		if issued && !errors.As(e, &refusal) {
			c.fence(g)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, e
	}
	var reply envelope
	if e = bulkJSON(f, &reply); e != nil {
		c.fence(g)
		return nil, e
	}
	if reply.Error != nil {
		if reply.Concurrency != nil && reply.Concurrency.Accounted {
			c.fence(g)
			return nil, errors.New("home: inconsistent admission refusal")
		}
		return nil, reply.Error.safeError()
	}
	tuple := reply.Concurrency
	if tuple == nil || !tuple.Accounted {
		c.fence(g)
		return nil, &Error{Type: "accounting_required", Message: "Home must configure a positive credential concurrency limit"}
	}
	if tuple.CredentialID != req.CredentialID || tuple.Model == "" || len(tuple.Model) > 1024 {
		c.fence(g)
		return nil, errors.New("home: invalid admission accounting tuple")
	}
	// Register the known admission before touching the credential. Releases use
	// the returned canonical tuple verbatim, never the caller's alias.
	leaseCtx, leaseCancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.work, leaseCancel)
	l := &Lease{client: c, generation: g, ctx: leaseCtx, cancel: leaseCancel, stop: stop, key: releaseKey{tuple.CredentialID, tuple.Model}, model: reply.Model, provider: reply.Provider, started: time.Now()}
	c.mu.Lock()
	if g.failed {
		c.mu.Unlock()
		leaseCancel()
		stop()
		return nil, ErrNotReady
	}
	g.active[l] = struct{}{}
	c.mu.Unlock()
	credential, e := reply.credential(req.CredentialID)
	if e == nil && (tuple.Model != reply.Model || reply.Model != req.Model || reply.Provider != "codex") {
		e = errors.New("home: unexpected provider or model remapping")
	}
	if e != nil {
		c.mu.Lock()
		g.accepting = false
		g.cancelWork()
		c.mu.Unlock()
		l.Finish(Result{Failed: true})
		c.flushReleases(g)
		c.fence(g)
		return nil, e
	}
	l.Credential = credential
	l.credential = credential
	if e = op.Err(); e != nil {
		l.Finish(Result{Failed: true})
		return nil, e
	}
	return l, nil
}
func (c *Client) Refresh(ctx context.Context, credentialID, observedAccessToken string) (Credential, error) {
	if !validateID(credentialID) {
		return Credential{}, errors.New("home: valid credential UUID is required")
	}
	g, op, done, e := c.begin(ctx)
	if e != nil {
		return Credential{}, e
	}
	defer done()
	request := map[string]string{"type": "refresh", "auth_index": credentialID}
	if observedAccessToken != "" {
		request["access_token_sha256"] = tokenHash(observedAccessToken)
	}
	raw, _ := json.Marshal(request)
	f, _, e := c.exchange(op, g, 35*time.Second, "GET", string(raw))
	if e != nil {
		return Credential{}, e
	}
	var reply envelope
	if e = bulkJSON(f, &reply); e != nil {
		return Credential{}, e
	}
	if reply.Error != nil {
		return Credential{}, reply.Error.safeError()
	}
	v, e := reply.credential(credentialID)
	if e != nil {
		return Credential{}, e
	}
	if e = op.Err(); e != nil {
		return Credential{}, e
	}
	return v, nil
}
func tokenHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
