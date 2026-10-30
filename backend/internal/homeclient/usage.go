package homeclient

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type usagePayload struct {
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"request_id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	AuthIndex string    `json:"auth_index"`
	AuthType  string    `json:"auth_type"`
	TokenHash string    `json:"access_token_sha256"`
	Endpoint  string    `json:"endpoint"`
	LatencyMS int64     `json:"latency_ms"`
	Stream    bool      `json:"stream"`
	Failed    bool      `json:"failed"`
	Fail      struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	} `json:"fail"`
	Tokens struct {
		Input     int64 `json:"input_tokens"`
		Output    int64 `json:"output_tokens"`
		Reasoning int64 `json:"reasoning_tokens"`
		Cached    int64 `json:"cached_tokens"`
		Total     int64 `json:"total_tokens"`
	} `json:"tokens"`
}

func nonnegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
func (l *Lease) usage(r Result) usagePayload {
	p := usagePayload{Timestamp: time.Now().UTC(), RequestID: r.RequestID, Provider: l.provider, Model: l.model, AuthIndex: l.credential.AuthIndex, AuthType: "oauth", TokenHash: tokenHash(l.credential.AccessToken), Endpoint: r.Endpoint, Stream: r.Stream, Failed: r.Failed || r.StatusCode >= 400, LatencyMS: nonnegative(r.Latency.Milliseconds())}
	if p.RequestID == "" || len(p.RequestID) > 128 {
		p.RequestID = uuid.NewString()
	}
	if p.LatencyMS == 0 {
		p.LatencyMS = time.Since(l.started).Milliseconds()
	}
	if p.Endpoint == "" {
		p.Endpoint = "/v1/responses"
	}
	if p.Endpoint != "/v1/responses" && p.Endpoint != "/v1/chat/completions" {
		p.Endpoint = "/v1/responses"
	}
	p.Fail.StatusCode = r.StatusCode
	// Arbitrary upstream bodies can contain tokens, prompts and PII. Only a
	// bounded machine error code crosses the control-plane reporting boundary.
	if p.Failed {
		code := r.ErrorCode
		if len(code) > 96 || strings.IndexFunc(code, func(ch rune) bool {
			return !(ch == '_' || ch == '-' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9')
		}) >= 0 {
			code = ""
		}
		if code == "" {
			code = "upstream_request_failed"
		}
		body := map[string]any{"type": code}
		if code == "usage_limit_reached" && r.StatusCode == 429 {
			if r.RetryAfter > 0 {
				body["resets_in_seconds"] = int64((r.RetryAfter-1)/time.Second) + 1
			}
			if !r.ResetAt.IsZero() && r.ResetAt.Unix() > 0 {
				body["resets_at"] = r.ResetAt.Unix()
			}
		}
		raw, _ := json.Marshal(map[string]any{"error": body})
		p.Fail.Body = string(raw)
	}
	p.Tokens.Input = nonnegative(r.InputTokens)
	p.Tokens.Output = nonnegative(r.OutputTokens)
	p.Tokens.Reasoning = nonnegative(r.ReasoningTokens)
	p.Tokens.Cached = nonnegative(r.CachedTokens)
	p.Tokens.Total = nonnegative(r.TotalTokens)
	return p
}
func (c *Client) usageLoop(g *generation) {
	for {
		select {
		case <-g.ctx.Done():
			return
		case p := <-g.usage:
			raw, _ := json.Marshal(p)
			_, _, _ = c.exchange(g.ctx, g, commandTimeout, "LPUSH", "usage", string(raw)) /* no retry: ACK loss is not idempotent */
		}
	}
}
