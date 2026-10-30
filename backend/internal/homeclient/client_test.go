package homeclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testID = "f35dafde-97f3-4d54-ab29-8694b2b96e44"
const testModel = "gpt-5.1-codex"
const testConfig = "credential-concurrency:\n  lifecycle-config-revision: 7\n  cpa-heartbeat-timeout: 250ms\n  cpa-cancel-bound: 300ms\n  release-flush-interval: 15ms\n  release-max-backoff: 40ms\n"

type record struct {
	args   []string
	member int
	conn   int
}
type fakeHome struct {
	t                            *testing.T
	listener                     net.Listener
	ca                           *x509.Certificate
	caKey                        *ecdsa.PrivateKey
	caPEM                        []byte
	mu                           sync.Mutex
	records                      []record
	members                      map[string]int
	active                       map[int]int
	seq                          map[int]int64
	conns                        map[net.Conn]bool
	nextConn, nextMember, issued int
	heartbeat                    bool
	rejectSubscribe              bool
	config                       string
	mutate                       func(map[string]any)
	blockAcquire                 chan struct{}
	dropRelease                  bool
	malformed                    bool
	usageFailure                 bool
}

func certificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, server bool) (tls.Certificate, []byte, []byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: serial.String()}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if ca == nil {
		cert.IsCA = true
		cert.BasicConstraintsValid = true
		cert.KeyUsage |= x509.KeyUsageCertSign
		ca = cert
		caKey = key
	} else if server {
		cert.DNSNames = []string{"home.test"}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, e := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
	if e != nil {
		t.Fatal(e)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	kp := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
	pair, e := tls.X509KeyPair(p, kp)
	if e != nil {
		t.Fatal(e)
	}
	parsed, _ := x509.ParseCertificate(der)
	return pair, p, kp, parsed, key
}
func newFake(t *testing.T) *fakeHome {
	t.Helper()
	_, caPEM, _, ca, caKey := certificate(t, nil, nil, false)
	server, _, _, _, _ := certificate(t, ca, caKey, true)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	l, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{server}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12})
	if e != nil {
		t.Fatal(e)
	}
	h := &fakeHome{t: t, listener: l, ca: ca, caKey: caKey, caPEM: caPEM, members: map[string]int{}, active: map[int]int{}, seq: map[int]int64{}, conns: map[net.Conn]bool{}, heartbeat: true, config: testConfig}
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			h.mu.Lock()
			h.nextConn++
			n := h.nextConn
			h.conns[c] = true
			h.mu.Unlock()
			go h.serve(c, n)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		for c := range h.conns {
			c.Close()
		}
	})
	return h
}
func (h *fakeHome) configForClient() Config {
	_, cert, key, _, _ := certificate(h.t, h.ca, h.caKey, false)
	dir := h.t.TempDir()
	for name, b := range map[string][]byte{"ca": h.caPEM, "cert": cert, "key": key} {
		if e := os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			h.t.Fatal(e)
		}
	}
	return Config{Address: h.listener.Addr().String(), ServerName: "home.test", CAFile: filepath.Join(dir, "ca"), CertFile: filepath.Join(dir, "cert"), KeyFile: filepath.Join(dir, "key"), APIKey: "service-secret"}
}
func readCommand(r *bufio.Reader) ([]string, error) {
	line, e := r.ReadString('\n')
	if e != nil {
		return nil, e
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("not array")
	}
	n, e := strconv.Atoi(strings.TrimSpace(line[1:]))
	if e != nil {
		return nil, e
	}
	var out []string
	for i := 0; i < n; i++ {
		line, e = r.ReadString('\n')
		if e != nil {
			return nil, e
		}
		size, e := strconv.Atoi(strings.TrimSpace(line[1:]))
		if e != nil || size < 0 || size > 2<<20 {
			return nil, fmt.Errorf("bad bulk")
		}
		b := make([]byte, size+2)
		if _, e = io.ReadFull(r, b); e != nil {
			return nil, e
		}
		out = append(out, string(b[:size]))
	}
	return out, nil
}
func bulk(c net.Conn, v any) { b, _ := json.Marshal(v); fmt.Fprintf(c, "$%d\r\n%s\r\n", len(b), b) }
func (h *fakeHome) serve(raw net.Conn, id int) {
	defer raw.Close()
	defer func() { h.mu.Lock(); delete(h.conns, raw); h.mu.Unlock() }()
	c := raw.(*tls.Conn)
	if e := c.Handshake(); e != nil {
		return
	}
	fp := hex.EncodeToString(c.ConnectionState().PeerCertificates[0].Raw)
	h.mu.Lock()
	member := h.members[fp]
	h.mu.Unlock()
	r := bufio.NewReader(c)
	for {
		args, e := readCommand(r)
		if e != nil {
			return
		}
		h.mu.Lock()
		h.records = append(h.records, record{append([]string(nil), args...), member, id})
		h.mu.Unlock()
		switch args[0] {
		case "GET":
			if args[1] == "config" {
				h.mu.Lock()
				cfg := h.config
				h.mu.Unlock()
				fmt.Fprintf(c, "$%d\r\n%s\r\n", len(cfg), cfg)
			} else {
				bulk(c, map[string]any{"auth_index": testID, "auth": map[string]any{"id": testID, "metadata": map[string]any{"access_token": "refreshed-token", "refresh_token": "must-not-escape", "account_id": "account"}}})
			}
		case "SUBSCRIBE":
			h.mu.Lock()
			reject := h.rejectSubscribe || h.members[fp] != 0
			if !reject {
				h.nextMember++
				member = h.nextMember
				h.members[fp] = member
			}
			h.mu.Unlock()
			if reject {
				io.WriteString(c, "-CPA certificate is already owned by an active membership\r\n")
				return
			}
			io.WriteString(c, "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n")
			defer func() { h.mu.Lock(); delete(h.members, fp); delete(h.active, member); h.mu.Unlock() }()
			closed := make(chan struct{})
			go func() { _, _ = r.ReadByte(); close(closed) }()
			ticker := time.NewTicker(30 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-closed:
					return
				case <-ticker.C:
					h.mu.Lock()
					on := h.heartbeat
					h.mu.Unlock()
					if on {
						if _, e := io.WriteString(c, "*2\r\n$4\r\npong\r\n$0\r\n\r\n"); e != nil {
							return
						}
					}
				}
			}
		case "PING":
			if member == 0 {
				io.WriteString(c, "-uncontrolled connection\r\n")
			} else {
				io.WriteString(c, "+PONG\r\n")
			}
		case "RPOP":
			h.mu.Lock()
			h.issued++
			block := h.blockAcquire
			malformed := h.malformed
			total := 0
			for _, n := range h.active {
				total += n
			}
			if total < 3 {
				h.active[member]++
			}
			mutate := h.mutate
			h.mu.Unlock()
			if block != nil {
				select {
				case <-block:
				case <-time.After(3 * time.Second):
				}
				return
			}
			if malformed {
				io.WriteString(c, "$5\r\n{nope\r\n")
				continue
			}
			if total >= 3 {
				bulk(c, map[string]any{"error": map[string]any{"type": "credential_concurrency_exceeded", "message": "full", "retryable": true, "retry_after_ms": 15}})
				continue
			}
			reply := map[string]any{"model": testModel, "provider": "codex", "auth_index": testID, "auth": map[string]any{"id": testID, "metadata": map[string]any{"access_token": "access-token", "account_id": "account", "email": "test@example.com", "expired": "2030-01-01T00:00:00Z"}}, "concurrency": map[string]any{"accounted": true, "credential_id": testID, "model": testModel}}
			if mutate != nil {
				mutate(reply)
			}
			bulk(c, reply)
		case "LPUSH":
			if args[1] == "concurrency-release" {
				var p struct {
					Seq int64 `json:"release_seq"`
				}
				json.Unmarshal([]byte(args[2]), &p)
				h.mu.Lock()
				delta := p.Seq - h.seq[member]
				if delta > 0 {
					h.active[member] -= int(delta)
					h.seq[member] = p.Seq
				}
				drop := h.dropRelease
				h.dropRelease = false
				h.mu.Unlock()
				if drop {
					return
				}
				io.WriteString(c, ":1\r\n")
			} else {
				h.mu.Lock()
				fail := h.usageFailure
				h.mu.Unlock()
				if fail {
					io.WriteString(c, "-usage unavailable\r\n")
				} else {
					io.WriteString(c, ":1\r\n")
				}
			}
		}
	}
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func startClient(t *testing.T, h *fakeHome) *Client {
	t.Helper()
	c, e := New(h.configForClient())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() { cancel(); c.Close() })
	eventually(t, c.Ready)
	return c
}
func acquire(t *testing.T, c *Client) *Lease {
	t.Helper()
	l, e := c.Acquire(context.Background(), Request{CredentialID: testID, Model: testModel, SessionID: "session"})
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func (h *fakeHome) count(command, key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.args[0] == command && (key == "" || r.args[1] == key) {
			n++
		}
	}
	return n
}

func TestTLSIdentityAndTrustRequired(t *testing.T) {
	h := newFake(t)
	cfg := h.configForClient()
	cfg.CertFile = ""
	if _, e := New(cfg); e == nil {
		t.Fatal("missing certificate accepted")
	}
	cfg = h.configForClient()
	_, wrong, _, _, _ := certificate(t, nil, nil, false)
	os.WriteFile(cfg.CAFile, wrong, 0600)
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)
	if c.Ready() || h.count("GET", "") != 0 {
		t.Fatal("untrusted server accepted")
	}
}
func TestBootstrapPinnedAdmissionAndFinishOnce(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	l := acquire(t, c)
	if l.Credential.ID != testID || l.Credential.AccountID != "account" || l.Credential.ExpiresAt.IsZero() {
		t.Fatalf("bad credential: %+v", l.Credential)
	}
	l.Finish(Result{StatusCode: 200, InputTokens: 12, OutputTokens: 3, TotalTokens: 15})
	l.Finish(Result{})
	eventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 && h.count("LPUSH", "usage") == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	var boot, sub, ping int
	for _, r := range h.records {
		switch r.args[0] {
		case "GET":
			boot = r.conn
		case "SUBSCRIBE":
			sub = r.conn
			if len(r.args) != 4 || r.args[2] != "7" {
				t.Fatalf("bad subscribe: %v", r.args)
			}
		case "PING":
			ping = r.conn
		case "RPOP":
			var p map[string]any
			json.Unmarshal([]byte(r.args[1]), &p)
			if len(r.args) != 2 || p["pinned_auth_id"] != testID || p["concurrency_protocol"] != float64(1) || p["headers"].(map[string]any)["authorization"] != "Bearer service-secret" {
				t.Fatalf("bad pin: %v", p)
			}
		}
	}
	if boot == ping || sub == ping || ping == 0 {
		t.Fatal("probe reused bootstrap/subscription connection")
	}
}
func TestTwoClientsShareThreeSlots(t *testing.T) {
	h := newFake(t)
	a, b := startClient(t, h), startClient(t, h)
	one, two, three := acquire(t, a), acquire(t, b), acquire(t, a)
	defer two.Finish(Result{})
	defer three.Finish(Result{})
	_, e := b.Acquire(context.Background(), Request{CredentialID: testID, Model: testModel})
	var homeErr *Error
	if !errors.As(e, &homeErr) || homeErr.Type != "credential_concurrency_exceeded" || homeErr.RetryAfter != 15*time.Millisecond {
		t.Fatalf("wrong refusal: %v", e)
	}
	if !b.Ready() {
		t.Fatal("deterministic refusal fenced membership")
	}
	one.Finish(Result{})
	eventually(t, func() bool { return h.count("LPUSH", "concurrency-release") > 0 })
	four := acquire(t, b)
	four.Finish(Result{})
}
func TestRejectInvalidAdmission(t *testing.T) {
	for _, kind := range []string{"wrong-id", "missing-accounting", "invalid-tuple", "remap", "mixed-error"} {
		t.Run(kind, func(t *testing.T) {
			h := newFake(t)
			h.mutate = func(v map[string]any) {
				switch kind {
				case "wrong-id":
					v["auth"].(map[string]any)["id"] = "other"
				case "missing-accounting":
					delete(v, "concurrency")
				case "invalid-tuple":
					v["concurrency"].(map[string]any)["credential_id"] = ""
				case "remap":
					v["model"] = "other"
				case "mixed-error":
					v["error"] = map[string]any{"type": "bad", "message": "bad"}
				}
			}
			c := startClient(t, h)
			l, e := c.Acquire(context.Background(), Request{CredentialID: testID, Model: testModel})
			if l != nil || e == nil {
				t.Fatalf("invalid admission accepted: %v", e)
			}
		})
	}
}
func TestAmbiguousAdmissionFencesAndDoesNotReplay(t *testing.T) {
	for _, kind := range []string{"cancel", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			h := newFake(t)
			c := startClient(t, h)
			l := acquire(t, c)
			h.mu.Lock()
			if kind == "cancel" {
				h.blockAcquire = make(chan struct{})
			} else {
				h.malformed = true
			}
			h.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, e := c.Acquire(ctx, Request{CredentialID: testID, Model: testModel})
			if e == nil {
				t.Fatal("ambiguous reply accepted")
			}
			select {
			case <-l.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("live lease not cancelled")
			}
			if c.Ready() {
				t.Fatal("ambiguous generation remains ready")
			}
			time.Sleep(80 * time.Millisecond)
			h.mu.Lock()
			issued := h.issued
			h.mu.Unlock()
			if issued != 2 {
				t.Fatalf("ambiguous RPOP replayed: %d", issued)
			}
			l.Finish(Result{Failed: true})
		})
	}
}
func TestHeartbeatLossWaitsForFinishedWorkBeforeFreshMembership(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	old := acquire(t, c)
	h.mu.Lock()
	h.heartbeat = false
	h.mu.Unlock()
	select {
	case <-old.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not cancel lease")
	}
	if c.Ready() {
		t.Fatal("lost heartbeat stays ready")
	}
	time.Sleep(350 * time.Millisecond)
	if h.count("SUBSCRIBE", "") != 1 {
		t.Fatal("reconnected over unfinished old execution")
	}
	old.Finish(Result{})
	h.mu.Lock()
	h.heartbeat = true
	h.rejectSubscribe = true
	h.mu.Unlock()
	eventually(t, func() bool { return h.count("SUBSCRIBE", "") > 1 })
	if c.Ready() {
		t.Fatal("duplicate refusal became ready")
	}
	h.mu.Lock()
	h.rejectSubscribe = false
	h.mu.Unlock()
	eventually(t, c.Ready)
	fresh := acquire(t, c)
	old.Finish(Result{})
	fresh.Finish(Result{})
	eventually(t, func() bool { return h.count("LPUSH", "concurrency-release") > 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "concurrency-release" {
			var p map[string]any
			json.Unmarshal([]byte(r.args[2]), &p)
			if p["release_seq"] != float64(1) {
				t.Fatalf("old ledger reused: %v", p)
			}
		}
	}
}
func TestReleaseRetryIsCumulativeAndUsageCannotBlockIt(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	a, b := acquire(t, c), acquire(t, c)
	h.mu.Lock()
	h.dropRelease = true
	h.usageFailure = true
	h.mu.Unlock()
	b.Finish(Result{Failed: true, StatusCode: 429, ErrorMessage: "rate limited"})
	eventually(t, func() bool { return h.count("LPUSH", "concurrency-release") >= 2 })
	a.Finish(Result{})
	eventually(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, s := range h.seq {
			if s == 2 {
				return true
			}
		}
		return false
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	var seqs []float64
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "concurrency-release" {
			var p map[string]any
			json.Unmarshal([]byte(r.args[2]), &p)
			seqs = append(seqs, p["release_seq"].(float64))
		}
	}
	if len(seqs) < 3 || seqs[0] != 1 || seqs[1] != 1 || seqs[len(seqs)-1] != 2 {
		t.Fatalf("bad release retry: %v", seqs)
	}
}
func TestRefreshUsesObservedHash(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	v, e := c.Refresh(context.Background(), testID, "observed-token")
	if e != nil || v.ID != testID || v.AccessToken != "refreshed-token" {
		t.Fatalf("bad refresh: %+v %v", v, e)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sum := sha256.Sum256([]byte("observed-token"))
	found := false
	for _, r := range h.records {
		if r.args[0] == "GET" && r.args[1] != "config" {
			found = true
			var p map[string]any
			json.Unmarshal([]byte(r.args[1]), &p)
			if p["type"] != "refresh" || p["auth_index"] != testID || p["access_token_sha256"] != hex.EncodeToString(sum[:]) {
				t.Fatalf("bad refresh request: %v", p)
			}
		}
	}
	if !found {
		t.Fatal("no refresh")
	}
}
func TestCloseCancelsPendingAndDrainsKnownRelease(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	l := acquire(t, c)
	go func() { <-l.Context().Done(); l.Finish(Result{}) }()
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	if c.Ready() {
		t.Fatal("closed client ready")
	}
	if h.count("LPUSH", "concurrency-release") == 0 {
		t.Fatal("graceful close lost release")
	}
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
}
func TestNoAdmissionBeforeRun(t *testing.T) {
	h := newFake(t)
	c, e := New(h.configForClient())
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if _, e = c.Acquire(context.Background(), Request{CredentialID: testID, Model: testModel}); !errors.Is(e, ErrNotReady) {
		t.Fatalf("got %v", e)
	}
}

func TestUsagePreservesCooldownWithoutSendingSecrets(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	l := acquire(t, c)
	// Treat the exported snapshot as caller-owned: reports must use the admitted token.
	l.Credential.AccessToken = "caller-mutated-secret"
	l.Finish(Result{Failed: true, StatusCode: 429, ErrorCode: "usage_limit_reached", ErrorMessage: "access-token prompt customer-secret", RetryAfter: 70 * time.Second, ResetAt: time.Unix(2000000000, 0), InputTokens: 10, OutputTokens: 2, TotalTokens: 12})
	eventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "usage" {
			var p struct {
				Hash string `json:"access_token_sha256"`
				Fail struct {
					Body   string `json:"body"`
					Status int    `json:"status_code"`
				} `json:"fail"`
			}
			json.Unmarshal([]byte(r.args[2]), &p)
			if strings.Contains(r.args[2], "customer-secret") || strings.Contains(r.args[2], "access-token") || strings.Contains(r.args[2], "caller-mutated-secret") {
				t.Fatal("usage leaked a secret")
			}
			var body struct {
				Error struct {
					Type    string `json:"type"`
					ResetAt int64  `json:"resets_at"`
					After   int64  `json:"resets_in_seconds"`
				} `json:"error"`
			}
			json.Unmarshal([]byte(p.Fail.Body), &body)
			if p.Hash != tokenHash("access-token") || p.Fail.Status != 429 || body.Error.Type != "usage_limit_reached" || body.Error.ResetAt != 2000000000 || body.Error.After != 70 {
				t.Fatalf("cooldown or token fence lost: %+v %+v", p, body)
			}
		}
	}
}
func TestCloseTerminatesIssuedPendingAcquire(t *testing.T) {
	h := newFake(t)
	c := startClient(t, h)
	h.mu.Lock()
	h.blockAcquire = make(chan struct{})
	h.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, e := c.Acquire(context.Background(), Request{CredentialID: testID, Model: testModel})
		done <- e
	}()
	eventually(t, func() bool { return h.count("RPOP", "") == 1 })
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("pending acquire succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pending acquire survived Close")
	}
}
func TestKnownInvalidCredentialReleasesCanonicalTuple(t *testing.T) {
	h := newFake(t)
	h.mutate = func(v map[string]any) {
		v["auth"].(map[string]any)["metadata"].(map[string]any)["access_token"] = ""
		v["concurrency"].(map[string]any)["model"] = "canonical-returned"
		v["model"] = "canonical-returned"
	}
	c := startClient(t, h)
	if _, e := c.Acquire(context.Background(), Request{CredentialID: testID, Model: testModel}); e == nil {
		t.Fatal("missing token accepted")
	}
	if h.count("LPUSH", "concurrency-release") == 0 {
		t.Fatal("known admission not released")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "concurrency-release" {
			var p map[string]any
			json.Unmarshal([]byte(r.args[2]), &p)
			if p["model"] != "canonical-returned" {
				t.Fatalf("release recomputed alias: %v", p)
			}
		}
	}
}
func TestProtocolAndLifecycleBounds(t *testing.T) {
	for _, raw := range []string{"$999999999\r\n", "*999999999\r\n", ":not-an-integer\r\n", "$1\r\naXX"} {
		if _, e := readFrame(bufio.NewReader(strings.NewReader(raw))); e == nil {
			t.Fatalf("accepted invalid frame %q", raw)
		}
	}
	for _, cfg := range []string{"{}", strings.Replace(testConfig, "revision: 7", "revision: 0", 1), strings.Replace(testConfig, "250ms", "0s", 1), strings.Replace(testConfig, "40ms", "1ms", 1)} {
		if _, e := parseLifecycle(cfg); e == nil {
			t.Fatalf("accepted invalid lifecycle %q", cfg)
		}
	}
}
