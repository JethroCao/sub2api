package service

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/homeclient"
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

const transportHomeTestID = "f35dafde-97f3-4d54-ab29-8694b2b96e44"
const transportHomeTestModel = "gpt-5.1-codex"
const transportHomeTestConfig = "credential-concurrency:\n  lifecycle-config-revision: 7\n  cpa-heartbeat-timeout: 250ms\n  cpa-cancel-bound: 300ms\n  release-flush-interval: 15ms\n  release-max-backoff: 40ms\n"

type transportHomeRecord struct {
	args   []string
	member int
	conn   int
}
type transportHomeFakeHome struct {
	t                            *testing.T
	listener                     net.Listener
	ca                           *x509.Certificate
	caKey                        *ecdsa.PrivateKey
	caPEM                        []byte
	mu                           sync.Mutex
	records                      []transportHomeRecord
	members                      map[string]int
	active                       map[int]int
	seq                          map[string]int64
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

func transportHomeCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, server bool) (tls.Certificate, []byte, []byte, *x509.Certificate, *ecdsa.PrivateKey) {
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
func transportHomeNewFake(t *testing.T) *transportHomeFakeHome {
	t.Helper()
	_, caPEM, _, ca, caKey := transportHomeCertificate(t, nil, nil, false)
	server, _, _, _, _ := transportHomeCertificate(t, ca, caKey, true)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	l, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{server}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12})
	if e != nil {
		t.Fatal(e)
	}
	h := &transportHomeFakeHome{t: t, listener: l, ca: ca, caKey: caKey, caPEM: caPEM, members: map[string]int{}, active: map[int]int{}, seq: map[string]int64{}, conns: map[net.Conn]bool{}, heartbeat: true, config: transportHomeTestConfig}
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
func (h *transportHomeFakeHome) configForClient() homeclient.Config {
	_, cert, key, _, _ := transportHomeCertificate(h.t, h.ca, h.caKey, false)
	dir := h.t.TempDir()
	for name, b := range map[string][]byte{"ca": h.caPEM, "cert": cert, "key": key} {
		if e := os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			h.t.Fatal(e)
		}
	}
	return homeclient.Config{Address: h.listener.Addr().String(), ServerName: "home.test", CAFile: filepath.Join(dir, "ca"), CertFile: filepath.Join(dir, "cert"), KeyFile: filepath.Join(dir, "key"), APIKey: "service-secret"}
}
func transportHomeReadCommand(r *bufio.Reader) ([]string, error) {
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
			return nil, fmt.Errorf("bad transportHomeBulk")
		}
		b := make([]byte, size+2)
		if _, e = io.ReadFull(r, b); e != nil {
			return nil, e
		}
		out = append(out, string(b[:size]))
	}
	return out, nil
}
func transportHomeBulk(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(c, "$%d\r\n%s\r\n", len(b), b)
}
func (h *transportHomeFakeHome) serve(raw net.Conn, id int) {
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
		args, e := transportHomeReadCommand(r)
		if e != nil {
			return
		}
		h.mu.Lock()
		h.records = append(h.records, transportHomeRecord{append([]string(nil), args...), member, id})
		h.mu.Unlock()
		switch args[0] {
		case "GET":
			if args[1] == "config" {
				h.mu.Lock()
				cfg := h.config
				h.mu.Unlock()
				fmt.Fprintf(c, "$%d\r\n%s\r\n", len(cfg), cfg)
			} else {
				transportHomeBulk(c, map[string]any{"auth_index": transportHomeTestID, "auth": map[string]any{"id": transportHomeTestID, "metadata": map[string]any{"access_token": "access-token", "refresh_token": "must-not-escape", "account_id": "account"}}})
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
				io.WriteString(c, "-CPA transportHomeCertificate is already owned by an active membership\r\n")
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
				transportHomeBulk(c, map[string]any{"error": map[string]any{"type": "credential_concurrency_exceeded", "message": "full", "retryable": true, "retry_after_ms": 15}})
				continue
			}
			var requested struct {
				Model string `json:"model"`
			}
			json.Unmarshal([]byte(args[1]), &requested)
			reply := map[string]any{"model": requested.Model, "provider": "codex", "auth_index": transportHomeTestID, "auth": map[string]any{"id": transportHomeTestID, "metadata": map[string]any{"access_token": "access-token", "account_id": "account", "email": "test@example.com", "expired": "2030-01-01T00:00:00Z"}}, "concurrency": map[string]any{"accounted": true, "credential_id": transportHomeTestID, "model": requested.Model}}
			if mutate != nil {
				mutate(reply)
			}
			transportHomeBulk(c, reply)
		case "LPUSH":
			if args[1] == "concurrency-release" {
				var p struct {
					Seq   int64  `json:"release_seq"`
					Model string `json:"model"`
				}
				json.Unmarshal([]byte(args[2]), &p)
				h.mu.Lock()
				key := strconv.Itoa(member) + ":" + p.Model
				delta := p.Seq - h.seq[key]
				if delta > 0 {
					h.active[member] -= int(delta)
					h.seq[key] = p.Seq
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
func transportHomeEventually(t *testing.T, f func() bool) {
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
func transportHomeStartClient(t *testing.T, h *transportHomeFakeHome) *homeclient.Client {
	t.Helper()
	c, e := homeclient.New(h.configForClient())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() { cancel(); c.Close() })
	transportHomeEventually(t, c.Ready)
	return c
}
func transportHomeAcquire(t *testing.T, c *homeclient.Client) *homeclient.Lease {
	t.Helper()
	l, e := c.Acquire(context.Background(), homeclient.Request{CredentialID: transportHomeTestID, Model: transportHomeTestModel, SessionID: "session"})
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func (h *transportHomeFakeHome) count(command, key string) int {
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
