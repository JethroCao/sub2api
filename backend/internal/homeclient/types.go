// Package homeclient implements the Home v1.1.7 control plane. Model traffic
// remains the caller's responsibility. Each running process needs a unique
// Home-issued client certificate; sharing a certificate fences membership.
package homeclient

import (
	"errors"
	"time"
)

// Config uses a pre-enrolled, process-specific mTLS identity and a Home service
// key. ServerName is verified against CAFile; insecure TLS is not supported.
type Config struct{ Address, ServerName, CAFile, CertFile, KeyFile, APIKey string }
type Request struct{ CredentialID, Model, SessionID string }
type Credential struct {
	ID, AuthIndex, AccessToken, AccountID, Email string
	ExpiresAt                                    time.Time
}

// Result reports one completed attempt. Failed or StatusCode >= 400 denotes
// failure. ErrorMessage is deliberately not transmitted; ErrorCode and the
// structured cooldown hints produce a bounded, secret-free failure body.
type Result struct {
	RetryAfter                                                            time.Duration
	ResetAt                                                               time.Time
	RequestID, Endpoint, ErrorCode, ErrorMessage                          string
	StatusCode                                                            int
	Failed, Stream                                                        bool
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens, TotalTokens int64
	Latency                                                               time.Duration
}

// Error is a deterministic Home refusal. Callers may respect RetryAfter within
// their own deadline; Acquire never automatically replays a command.
type Error struct {
	Type, Message string
	Retryable     bool
	RetryAfter    time.Duration
}

func (e *Error) Error() string { return "home: " + e.Type + ": " + e.Message }

var (
	ErrNotReady     = errors.New("home: membership is not ready")
	ErrClosed       = errors.New("home: client is closed")
	ErrDrainTimeout = errors.New("home: executions did not finish within cancel bound")
)
