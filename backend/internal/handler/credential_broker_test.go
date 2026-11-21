package handler

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type brokerHTTPStub struct {
	enabled       bool
	calls         int
	err           error
	key, revision string
	id            int64
}

func (s *brokerHTTPStub) Enabled() bool { return s.enabled }
func (s *brokerHTTPStub) Issue(_ context.Context, key string, id int64, revision string) (*service.CredentialBrokerToken, error) {
	s.calls++
	s.key = key
	s.id = id
	s.revision = revision
	if s.err != nil {
		return nil, s.err
	}
	return &service.CredentialBrokerToken{AccountID: id, AccessToken: "fixture-at", ChatGPTAccountID: "upstream-id", ExpiresAt: time.Now().Add(time.Hour), Revision: strings.Repeat("a", 64)}, nil
}
func brokerHTTP(t *testing.T, s *brokerHTTPStub, cfg config.CredentialBrokerConfig, body, auth, remote, forwarded string, secure bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewCredentialBrokerHandler(nil, &config.Config{CredentialBroker: cfg})
	h.broker = s
	r := gin.New()
	r.POST("/api/v1/credential-broker/openai/:id/token", h.Issue)
	req := httptest.NewRequest("POST", "/api/v1/credential-broker/openai/4/token", strings.NewReader(body))
	req.RemoteAddr = remote
	if secure {
		req.TLS = &tls.ConnectionState{}
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if forwarded != "" {
		req.Header.Set("X-Forwarded-Proto", forwarded)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", w.Header().Get("Pragma"))
	return w
}
func TestCredentialBrokerHTTPTransportAndParsing(t *testing.T) {
	for _, tt := range []struct {
		name, body, auth, remote, forwarded string
		secure, enabled                     bool
		want, calls                         int
	}{
		{"disabled", "{}", "Bearer fixture-key", "192.0.2.1:12", "", true, false, 404, 0},
		{"cleartext", "{}", "Bearer fixture-key", "192.0.2.1:12", "", false, true, 403, 0},
		{"spoofed-proxy", "{}", "Bearer fixture-key", "192.0.2.1:12", "https", false, true, 403, 0},
		{"trusted-proxy", "{}", "Bearer fixture-key", "127.0.0.1:12", "https", false, true, 200, 1},
		{"ambiguous-proxy", "{}", "Bearer fixture-key", "127.0.0.1:12", "https,http", false, true, 403, 0},
		{"missing-auth", "{}", "", "192.0.2.1:12", "", true, true, 401, 0},
		{"bad-auth", "{}", "Basic fixture-key", "192.0.2.1:12", "", true, true, 401, 0},
		{"unknown-field", "{\"refresh_token\":\"NEVER-ECHO\"}", "Bearer fixture-key", "192.0.2.1:12", "", true, true, 400, 0},
		{"oversize", strings.Repeat(" ", 2049), "Bearer fixture-key", "192.0.2.1:12", "", true, true, 413, 0},
		{"multiple-objects", "{}{}", "Bearer fixture-key", "192.0.2.1:12", "", true, true, 400, 0},
		{"null", "null", "Bearer fixture-key", "192.0.2.1:12", "", true, true, 400, 0},
		{"empty", "", "Bearer fixture-key", "192.0.2.1:12", "", true, true, 200, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &brokerHTTPStub{enabled: tt.enabled}
			w := brokerHTTP(t, s, config.CredentialBrokerConfig{TrustedProxyCIDRs: []string{"127.0.0.1/32"}}, tt.body, tt.auth, tt.remote, tt.forwarded, tt.secure)
			require.Equal(t, tt.want, w.Code)
			require.Equal(t, tt.calls, s.calls)
			require.NotContains(t, w.Body.String(), "NEVER-ECHO")
		})
	}
}
func TestCredentialBrokerHTTPProjectionAndErrorRedaction(t *testing.T) {
	s := &brokerHTTPStub{enabled: true}
	revision := strings.Repeat("b", 64)
	w := brokerHTTP(t, s, config.CredentialBrokerConfig{}, `{"rejected_revision":"`+revision+`"}`, "Bearer fixture-key", "192.0.2.1:12", "", true)
	require.Equal(t, 200, w.Code)
	require.Equal(t, int64(4), s.id)
	require.Equal(t, "fixture-key", s.key)
	require.Equal(t, revision, s.revision)
	require.NotContains(t, w.Body.String(), "refresh_token")
	require.Contains(t, w.Body.String(), "fixture-at")
	s.err = errors.New("NEVER-ECHO-SECRET")
	w = brokerHTTP(t, s, config.CredentialBrokerConfig{}, "{}", "Bearer fixture-key", "192.0.2.1:12", "", true)
	require.Equal(t, 503, w.Code)
	require.NotContains(t, w.Body.String(), "NEVER-ECHO")
	s.err = &service.CredentialBrokerError{Status: 429, Code: "refresh_throttled", RetryAfter: 30}
	w = brokerHTTP(t, s, config.CredentialBrokerConfig{}, "{}", "Bearer fixture-key", "192.0.2.1:12", "", true)
	require.Equal(t, 429, w.Code)
	require.Equal(t, "30", w.Header().Get("Retry-After"))
}
