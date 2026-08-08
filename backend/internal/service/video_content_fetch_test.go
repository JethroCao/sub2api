package service

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVideoContentFetcherRejectsLiteralPrivateDestination(t *testing.T) {
	fetcher := newVideoContentFetcher(videoContentResolverStub{}, &videoContentDialerStub{}, videoContentFetchOptions{})

	_, err := fetcher.Fetch(context.Background(), "https://127.0.0.1/video.mp4", "")

	require.ErrorIs(t, err, ErrVideoContentUnsafeURL)
}

func TestVideoContentFetcherRejectsPrivateDNSAnswerBeforeDial(t *testing.T) {
	dialer := &videoContentDialerStub{}
	fetcher := newVideoContentFetcher(videoContentResolverStub{answers: map[string][]net.IP{
		"media.example": {net.ParseIP("10.0.0.9")},
	}}, dialer, videoContentFetchOptions{})

	_, err := fetcher.Fetch(context.Background(), "https://media.example/video.mp4", "")

	require.ErrorIs(t, err, ErrVideoContentUnsafeURL)
	require.Empty(t, dialer.addresses())
}

func TestVideoContentFetcherBindsDialToValidatedResolvedAddressAndForwardsRange(t *testing.T) {
	var gotRange string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 2-4/6")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "cde")
	}))
	defer server.Close()

	localAddress := mustVideoTestServerAddress(t, server.URL)
	dialer := &videoContentDialerStub{target: localAddress}
	fetcher := newVideoContentFetcher(videoContentResolverStub{answers: map[string][]net.IP{
		"media.example": {net.ParseIP("93.184.216.34")},
	}}, dialer, videoContentFetchOptions{TLSConfig: &tls.Config{InsecureSkipVerify: true}}) //nolint:gosec // test-only TLS endpoint

	response, err := fetcher.Fetch(context.Background(), "https://media.example/video.mp4", "bytes=2-4")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Equal(t, "cde", string(body))
	require.Equal(t, "bytes=2-4", gotRange)
	require.Equal(t, []string{"93.184.216.34:443"}, dialer.addresses())
}

func TestVideoContentFetcherRevalidatesEveryRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "first.example" {
			http.Redirect(w, r, "https://second.example/video.mp4", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "video")
	}))
	defer server.Close()

	dialer := &videoContentDialerStub{target: mustVideoTestServerAddress(t, server.URL)}
	resolver := videoContentResolverStub{answers: map[string][]net.IP{
		"first.example":  {net.ParseIP("93.184.216.34")},
		"second.example": {net.ParseIP("93.184.216.35")},
	}}
	fetcher := newVideoContentFetcher(resolver, dialer, videoContentFetchOptions{TLSConfig: &tls.Config{InsecureSkipVerify: true}}) //nolint:gosec // test-only TLS endpoint

	response, err := fetcher.Fetch(context.Background(), "https://first.example/start", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)

	require.Equal(t, []string{"93.184.216.34:443", "93.184.216.35:443"}, dialer.addresses())
}

func TestVideoContentFetcherRejectsRedirectToPrivateDestination(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	dialer := &videoContentDialerStub{target: mustVideoTestServerAddress(t, server.URL)}
	fetcher := newVideoContentFetcher(videoContentResolverStub{answers: map[string][]net.IP{
		"media.example": {net.ParseIP("93.184.216.34")},
	}}, dialer, videoContentFetchOptions{TLSConfig: &tls.Config{InsecureSkipVerify: true}}) //nolint:gosec // test-only TLS endpoint

	_, err := fetcher.Fetch(context.Background(), "https://media.example/start", "")

	require.ErrorIs(t, err, ErrVideoContentUnsafeURL)
	require.Equal(t, []string{"93.184.216.34:443"}, dialer.addresses())
}

func TestVideoContentFetcherCapsRedirectsResponseBytesAndDuration(t *testing.T) {
	t.Run("redirects", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://media.example/again", http.StatusFound)
		}))
		defer server.Close()
		fetcher := newVideoContentFetcher(videoContentResolverStub{answers: map[string][]net.IP{
			"media.example": {net.ParseIP("93.184.216.34")},
		}}, &videoContentDialerStub{target: mustVideoTestServerAddress(t, server.URL)}, videoContentFetchOptions{
			MaxRedirects: 1, TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only TLS endpoint
		})
		_, err := fetcher.Fetch(context.Background(), "https://media.example/start", "")
		require.ErrorIs(t, err, ErrVideoContentTooManyRedirects)
	})

	t.Run("bytes", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			_, _ = io.WriteString(w, "123456")
		}))
		defer server.Close()
		fetcher := newVideoContentFetcher(videoContentResolverStub{answers: map[string][]net.IP{
			"media.example": {net.ParseIP("93.184.216.34")},
		}}, &videoContentDialerStub{target: mustVideoTestServerAddress(t, server.URL)}, videoContentFetchOptions{
			MaxBytes: 5, TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only TLS endpoint
		})
		response, err := fetcher.Fetch(context.Background(), "https://media.example/video", "")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
		body, err := io.ReadAll(response.Body)
		require.ErrorIs(t, err, ErrVideoContentTooLarge)
		require.Len(t, body, 5)
	})

	t.Run("duration", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = io.WriteString(w, "late")
		}))
		defer server.Close()
		fetcher := newVideoContentFetcher(videoContentResolverStub{answers: map[string][]net.IP{
			"media.example": {net.ParseIP("93.184.216.34")},
		}}, &videoContentDialerStub{target: mustVideoTestServerAddress(t, server.URL)}, videoContentFetchOptions{
			Timeout: 20 * time.Millisecond, TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only TLS endpoint
		})
		_, err := fetcher.Fetch(context.Background(), "https://media.example/video", "")
		require.Error(t, err)
	})
}

type videoContentResolverStub struct{ answers map[string][]net.IP }

func (r videoContentResolverStub) LookupIP(_ context.Context, _ string, host string) ([]net.IP, error) {
	return append([]net.IP(nil), r.answers[host]...), nil
}

type videoContentDialerStub struct {
	target string
	mu     sync.Mutex
	dials  []string
}

func (d *videoContentDialerStub) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, address)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, d.target)
}

func (d *videoContentDialerStub) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dials...)
}

func mustVideoTestServerAddress(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Host)
	return parsed.Host
}
