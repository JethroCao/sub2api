package homeclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const commandTimeout = 3 * time.Second
const maxFrame = 4 << 20

type frame struct {
	kind   byte
	text   string
	number int64
	array  []frame
}
type respError struct{}

func (respError) Error() string                { return "home: command refused" }
func readFrame(r *bufio.Reader) (frame, error) { budget := maxFrame; return readPart(r, 0, &budget) }
func readPart(r *bufio.Reader, depth int, budget *int) (frame, error) {
	if depth > 4 {
		return frame{}, errors.New("home: RESP nesting limit")
	}
	line, e := r.ReadSlice('\n')
	if e != nil {
		return frame{}, errors.New("home: incomplete RESP frame")
	}
	*budget -= len(line)
	if len(line) < 3 || *budget < 0 || line[len(line)-2] != '\r' {
		return frame{}, errors.New("home: invalid RESP frame")
	}
	f := frame{kind: line[0]}
	body := string(line[1 : len(line)-2])
	switch f.kind {
	case '+':
		f.text = body
	case '-':
		return frame{}, respError{}
	case ':':
		f.number, e = strconv.ParseInt(body, 10, 64)
	case '$', '*':
		var n int64
		n, e = strconv.ParseInt(body, 10, 64)
		if e != nil || n < 0 || n > int64(*budget) {
			return frame{}, errors.New("home: invalid RESP length")
		}
		if f.kind == '$' {
			b := make([]byte, int(n)+2)
			_, e = io.ReadFull(r, b)
			*budget -= len(b)
			if e == nil && (b[len(b)-2] != '\r' || b[len(b)-1] != '\n') {
				e = errors.New("home: invalid bulk terminator")
			}
			f.text = string(b[:n])
		} else {
			if n > 128 {
				return frame{}, errors.New("home: RESP array limit")
			}
			for i := int64(0); i < n && e == nil; i++ {
				var item frame
				item, e = readPart(r, depth+1, budget)
				f.array = append(f.array, item)
			}
		}
	default:
		e = errors.New("home: unsupported RESP frame")
	}
	if e != nil {
		return frame{}, errors.New("home: invalid RESP frame")
	}
	return f, nil
}
func writeCommand(c net.Conn, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, s := range args {
		if len(s) > maxFrame {
			return errors.New("home: command too large")
		}
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(s), s)
	}
	_, e := io.Copy(c, strings.NewReader(b.String()))
	return e
}
func tlsConfig(cfg Config) (*tls.Config, error) {
	if strings.TrimSpace(cfg.Address) == "" || strings.TrimSpace(cfg.ServerName) == "" || cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("home: address, server name, CA, client certificate/key and service API key are required")
	}
	if _, _, e := net.SplitHostPort(cfg.Address); e != nil {
		return nil, errors.New("home: address must include host and port")
	}
	roots := x509.NewCertPool()
	ca, e := os.ReadFile(cfg.CAFile)
	if e != nil || !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("home: cannot load trusted CA")
	}
	cert, e := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if e != nil || len(cert.Certificate) == 0 {
		return nil, errors.New("home: cannot load client identity")
	}
	leaf, e := x509.ParseCertificate(cert.Certificate[0])
	if e != nil || leaf.Subject.CommonName == "" || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("home: invalid or expired client identity")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName, RootCAs: roots, Certificates: []tls.Certificate{cert}}, nil
}
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: commandTimeout}, Config: c.tls}
	conn, e := d.DialContext(ctx, "tcp", c.cfg.Address)
	if e != nil {
		return nil, errors.New("home: TLS connection failed")
	}
	return conn, nil
}

// exchange never retries. issued is conservative: any attempted command write
// may have admitted a request even if the transport returns an error.
func (c *Client) exchange(ctx context.Context, g *generation, timeout time.Duration, args ...string) (frame, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, e := c.dial(ctx)
	if e != nil {
		return frame{}, false, e
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c.mu.Lock()
	if g.failed {
		c.mu.Unlock()
		return frame{}, false, ErrNotReady
	}
	g.conns[conn] = struct{}{}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(g.conns, conn); c.mu.Unlock() }()
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	if e = ctx.Err(); e != nil {
		return frame{}, false, e
	}
	if e = writeCommand(conn, args...); e != nil {
		return frame{}, true, errors.New("home: command write failed")
	}
	f, e := readFrame(bufio.NewReader(conn))
	return f, true, e
}
func bulkJSON(f frame, v any) error {
	if f.kind != '$' || json.Unmarshal([]byte(f.text), v) != nil {
		return errors.New("home: invalid JSON response")
	}
	return nil
}

type lifecycle struct {
	Revision    int64         `yaml:"lifecycle-config-revision"`
	Heartbeat   time.Duration `yaml:"cpa-heartbeat-timeout"`
	CancelBound time.Duration `yaml:"cpa-cancel-bound"`
	Flush       time.Duration `yaml:"release-flush-interval"`
	MaxBackoff  time.Duration `yaml:"release-max-backoff"`
}

func parseLifecycle(s string) (lifecycle, error) {
	var doc struct {
		Legacy      *lifecycle `yaml:"credential-concurrency"`
		Credentials struct {
			Concurrency *lifecycle `yaml:"concurrency"`
		} `yaml:"credentials"`
	}
	if len(s) > maxFrame || yaml.Unmarshal([]byte(s), &doc) != nil {
		return lifecycle{}, errors.New("home: invalid lifecycle config")
	}
	p := doc.Legacy
	if p == nil {
		p = doc.Credentials.Concurrency
	}
	if p == nil || p.Revision < 1 || p.Heartbeat < time.Millisecond || p.Heartbeat > time.Minute || p.CancelBound < time.Millisecond || p.CancelBound > time.Minute || p.Flush < time.Millisecond || p.Flush > time.Minute || p.MaxBackoff < p.Flush || p.MaxBackoff > time.Minute {
		return lifecycle{}, errors.New("home: invalid lifecycle bounds")
	}
	return *p, nil
}
