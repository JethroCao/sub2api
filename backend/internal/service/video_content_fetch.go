package service

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultVideoContentMaxRedirects = 3
	defaultVideoContentMaxBytes     = int64(1 << 30)
	defaultVideoContentTimeout      = 2 * time.Minute
)

var (
	ErrVideoContentUnsafeURL        = errors.New("unsafe video content URL")
	ErrVideoContentTooManyRedirects = errors.New("too many video content redirects")
	ErrVideoContentTooLarge         = errors.New("video content exceeds size limit")
)

type videoContentResolver interface {
	LookupIP(context.Context, string, string) ([]net.IP, error)
}

type videoContentDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type videoContentFetchOptions struct {
	MaxRedirects int
	MaxBytes     int64
	Timeout      time.Duration
	TLSConfig    *tls.Config
}

// VideoContentFetcher opens only trusted public HTTPS result URLs. Its dialer
// is repository-owned: callers supply a URL and Range value, never a custom
// DialContext that could bypass destination validation.
type VideoContentFetcher struct {
	resolver videoContentResolver
	dialer   videoContentDialer
	options  videoContentFetchOptions
}

func NewVideoContentFetcher() *VideoContentFetcher {
	return newVideoContentFetcher(net.DefaultResolver, &net.Dialer{Timeout: 10 * time.Second}, videoContentFetchOptions{})
}

func newVideoContentFetcher(resolver videoContentResolver, dialer videoContentDialer, options videoContentFetchOptions) *VideoContentFetcher {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	if options.MaxRedirects <= 0 {
		options.MaxRedirects = defaultVideoContentMaxRedirects
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultVideoContentMaxBytes
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultVideoContentTimeout
	}
	return &VideoContentFetcher{resolver: resolver, dialer: dialer, options: options}
}

func (f *VideoContentFetcher) Fetch(ctx context.Context, rawURL, rangeHeader string) (*http.Response, error) {
	if f == nil || f.resolver == nil || f.dialer == nil {
		return nil, ErrVideoContentUnsafeURL
	}
	initialURL, err := validateVideoContentURL(rawURL)
	if err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, f.options.Timeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, initialURL.String(), nil)
	if err != nil {
		cancel()
		return nil, ErrVideoContentUnsafeURL
	}
	request.Header.Set("Accept", "*/*")
	if value := strings.TrimSpace(rangeHeader); value != "" {
		request.Header.Set("Range", value)
	}

	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: f.options.Timeout,
		TLSClientConfig:       cloneVideoContentTLSConfig(f.options.TLSConfig),
	}
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, ErrVideoContentUnsafeURL
		}
		addresses, resolveErr := f.resolver.LookupIP(dialCtx, "ip", strings.TrimSuffix(host, "."))
		if resolveErr != nil || len(addresses) == 0 {
			return nil, ErrVideoContentUnsafeURL
		}
		for _, addressIP := range addresses {
			if addressIP == nil || isBlockedVideoIP(addressIP) {
				return nil, ErrVideoContentUnsafeURL
			}
		}
		return f.dialer.DialContext(dialCtx, network, net.JoinHostPort(addresses[0].String(), port))
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) > f.options.MaxRedirects {
				return ErrVideoContentTooManyRedirects
			}
			_, redirectErr := validateVideoContentURL(next.URL.String())
			return redirectErr
		},
	}
	response, err := client.Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		cancel()
		return nil, err
	}
	if response.ContentLength > f.options.MaxBytes {
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		cancel()
		return nil, ErrVideoContentTooLarge
	}
	response.Body = &boundedVideoContentBody{
		body: response.Body, remaining: f.options.MaxBytes,
		cancel: cancel, closeIdle: transport.CloseIdleConnections,
	}
	return response, nil
}

func validateVideoContentURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrVideoContentUnsafeURL
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.Contains(host, "%") || isNumericVideoHostname(host) {
		return nil, ErrVideoContentUnsafeURL
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value <= 0 || value > 65535 {
			return nil, ErrVideoContentUnsafeURL
		}
	}
	if literal := net.ParseIP(host); literal != nil && isBlockedVideoIP(literal) {
		return nil, ErrVideoContentUnsafeURL
	}
	return parsed, nil
}

func cloneVideoContentTLSConfig(config *tls.Config) *tls.Config {
	if config == nil {
		return nil
	}
	return config.Clone()
}

type boundedVideoContentBody struct {
	body      io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
	closeIdle func()
	closed    bool
}

func (b *boundedVideoContentBody) Read(buffer []byte) (int, error) {
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			return 0, ErrVideoContentTooLarge
		}
		return 0, err
	}
	if int64(len(buffer)) > b.remaining {
		buffer = buffer[:b.remaining]
	}
	n, err := b.body.Read(buffer)
	b.remaining -= int64(n)
	return n, err
}

func (b *boundedVideoContentBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	err := b.body.Close()
	if b.cancel != nil {
		b.cancel()
	}
	if b.closeIdle != nil {
		b.closeIdle()
	}
	return err
}
