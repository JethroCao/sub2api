package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// CredentialBrokerToken is deliberately not an Account/credentials document.
type CredentialBrokerToken struct {
	AccountID        int64     `json:"account_id"`
	AccessToken      string    `json:"access_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	ChatGPTAccountID string    `json:"chatgpt_account_id"`
	Revision         string    `json:"revision"`
}
type CredentialBrokerError struct {
	Status     int
	Code       string
	RetryAfter int
}

func (e *CredentialBrokerError) Error() string { return e.Code }
func brokerError(status int, code string, retry int) *CredentialBrokerError {
	return &CredentialBrokerError{Status: status, Code: code, RetryAfter: retry}
}

func brokerRetrySeconds(d time.Duration) int {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		return 1
	}
	return n
}

type brokerGrant struct {
	id       string
	digest   [32]byte
	accounts map[int64]bool
}

// CredentialBrokerLimiter is implemented by the shared Redis infrastructure.
type CredentialBrokerLimiter interface {
	Allow(context.Context, string, int, time.Duration) (bool, time.Duration, error)
}
type CredentialBrokerService struct {
	enabled  bool
	grants   []brokerGrant
	accounts AccountRepository
	refresh  *OAuthRefreshAPI
	executor OAuthRefreshExecutor
	cache    GeminiTokenCache
	limiter  CredentialBrokerLimiter
}

func NewCredentialBrokerService(repo AccountRepository, refresh *OAuthRefreshAPI, oauth *OpenAIOAuthService, cache GeminiTokenCache, limiter CredentialBrokerLimiter, cfg *config.Config) *CredentialBrokerService {
	s := &CredentialBrokerService{accounts: repo, refresh: refresh, executor: NewOpenAITokenRefresher(oauth, repo), cache: cache, limiter: limiter}
	if cfg == nil || !cfg.CredentialBroker.Enabled || cfg.CredentialBroker.Validate() != nil {
		return s
	}
	s.enabled = true
	for _, c := range cfg.CredentialBroker.Clients {
		g := brokerGrant{id: c.ID, accounts: map[int64]bool{}}
		b, _ := hex.DecodeString(c.KeySHA256)
		copy(g.digest[:], b)
		for _, id := range c.AccountIDs {
			g.accounts[id] = true
		}
		s.grants = append(s.grants, g)
	}
	return s
}
func (s *CredentialBrokerService) Enabled() bool { return s != nil && s.enabled }
func (s *CredentialBrokerService) authenticate(key string) *brokerGrant {
	if len(key) < 32 || len(key) > 512 {
		return nil
	}
	digest := sha256.Sum256([]byte(key))
	var match *brokerGrant
	for i := range s.grants {
		if subtle.ConstantTimeCompare(digest[:], s.grants[i].digest[:]) == 1 {
			match = &s.grants[i]
		}
	}
	return match
}
func brokerEligible(a *Account) bool {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth || a.HasHomeBinding() || a.IsCredentialShadow() || a.IsOpenAIPersonalAccessToken() || !a.IsSchedulable() || strings.TrimSpace(a.GetOpenAIRefreshToken()) == "" {
		return false
	}
	for _, key := range []string{"auth_mode", "openai_auth_mode"} {
		if mode := strings.ToLower(strings.TrimSpace(a.GetCredential(key))); mode != "" && mode != "oauth" {
			return false
		}
	}
	for k, v := range a.Credentials {
		if (strings.HasPrefix(k, "agent_") || k == "task_id") && v != nil && v != "" {
			return false
		}
	}
	return true
}
func brokerRevision(a *Account) string {
	v := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", a.ID, a.GetCredential("access_token"), a.GetCredential("chatgpt_account_id"))))
	return hex.EncodeToString(v[:])
}
func validBrokerRevision(v string) bool {
	if len(v) != 64 || v != strings.ToLower(v) {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// Issue authorizes before repository access. RejectedRevision is an observation,
// not an unconditional force-refresh switch: a newer durable token wins.
func (s *CredentialBrokerService) Issue(ctx context.Context, key string, id int64, rejectedRevision string) (out *CredentialBrokerToken, issueErr error) {
	if !s.Enabled() {
		return nil, brokerError(404, "not_found", 0)
	}
	grant := s.authenticate(key)
	if grant == nil {
		return nil, brokerError(401, "unauthorized", 0)
	}
	if !grant.accounts[id] {
		return nil, brokerError(404, "not_found", 0)
	}
	defer func() {
		code := "ok"
		var e *CredentialBrokerError
		if errors.As(issueErr, &e) {
			code = e.Code
		}
		slog.Info("credential_broker.issue", "client_id", grant.id, "account_id", id, "result", code)
	}()
	if rejectedRevision != "" && !validBrokerRevision(rejectedRevision) {
		return nil, brokerError(400, "invalid_request", 0)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if s.limiter == nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	allowed, retry, err := s.limiter.Allow(ctx, "requests:"+grant.id, 60, time.Minute)
	if err != nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	if !allowed {
		return nil, brokerError(429, "rate_limited", brokerRetrySeconds(retry))
	}
	if s.accounts == nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	a, err := s.accounts.GetByID(ctx, id)
	if errors.Is(err, ErrAccountNotFound) {
		return nil, brokerError(404, "not_found", 0)
	}
	if err != nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	if a == nil || a.ID != id || !brokerEligible(a) {
		return nil, brokerError(503, "credential_unavailable", 5)
	}
	executor := &brokerRefreshExecutor{base: s.executor, limiter: s.limiter, rejected: rejectedRevision}
	if executor.NeedsRefresh(a, time.Minute) {
		if s.refresh == nil || s.executor == nil || s.cache == nil {
			return nil, brokerError(503, "broker_unavailable", 1)
		}
		result, refreshErr := s.refresh.RefreshIfNeeded(withCredentialBrokerRefresh(WithNativeCredentialMutation(withOAuthRefreshRequestPath(ctx))), a, executor, time.Minute)
		if refreshErr != nil {
			var limited *CredentialBrokerError
			if errors.As(refreshErr, &limited) {
				return nil, limited
			}
			if errors.Is(refreshErr, ErrCredentialBrokerStateChanged) {
				return nil, brokerError(503, "credential_unavailable", 1)
			}
			return nil, brokerError(502, "credential_refresh_failed", 1)
		}
		if result == nil || result.LockHeld {
			return nil, brokerError(503, "refresh_in_progress", 1)
		}
	}
	// Also heal a prior post-commit Redis failure on ordinary token fetches.
	if ctx.Err() != nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	if s.cache == nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	if err := s.cache.DeleteAccessToken(ctx, OpenAITokenCacheKey(a)); err != nil {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	// Never project an old snapshot after a revocation/rebind/reauthorization.
	a, err = s.accounts.GetByID(ctx, id)
	if err != nil || a == nil || a.ID != id {
		return nil, brokerError(503, "broker_unavailable", 1)
	}
	expires := a.GetCredentialAsTime("expires_at")
	if !brokerEligible(a) || expires == nil || !expires.After(time.Now()) || strings.TrimSpace(a.GetCredential("access_token")) == "" || strings.TrimSpace(a.GetCredential("chatgpt_account_id")) == "" {
		return nil, brokerError(503, "credential_unavailable", 1)
	}
	return &CredentialBrokerToken{AccountID: id, AccessToken: a.GetCredential("access_token"), ExpiresAt: expires.UTC(), ChatGPTAccountID: a.GetCredential("chatgpt_account_id"), Revision: brokerRevision(a)}, nil
}

type brokerRefreshExecutor struct {
	base     OAuthRefreshExecutor
	limiter  CredentialBrokerLimiter
	rejected string
}

func (e *brokerRefreshExecutor) CacheKey(a *Account) string { return OpenAITokenCacheKey(a) }
func (e *brokerRefreshExecutor) CanRefresh(a *Account) bool {
	return e.base != nil && brokerEligible(a) && e.base.CanRefresh(a)
}
func (e *brokerRefreshExecutor) naturalRefresh(a *Account, w time.Duration) bool {
	exp := a.GetCredentialAsTime("expires_at")
	return exp == nil || time.Until(*exp) <= w || strings.TrimSpace(a.GetCredential("access_token")) == ""
}
func (e *brokerRefreshExecutor) NeedsRefresh(a *Account, w time.Duration) bool {
	return a != nil && (e.naturalRefresh(a, w) || (e.rejected != "" && brokerRevision(a) == e.rejected))
}
func (e *brokerRefreshExecutor) Refresh(ctx context.Context, a *Account) (map[string]any, error) {
	if !e.CanRefresh(a) {
		return nil, ErrCredentialBrokerStateChanged
	}
	if e.rejected != "" && brokerRevision(a) == e.rejected && !e.naturalRefresh(a, time.Minute) {
		ok, retry, err := e.limiter.Allow(ctx, fmt.Sprintf("forced:%d", a.ID), 1, 30*time.Second)
		if err != nil {
			return nil, brokerError(503, "broker_unavailable", 1)
		}
		if !ok {
			return nil, brokerError(429, "refresh_throttled", brokerRetrySeconds(retry))
		}
	}
	return e.base.Refresh(withCredentialBrokerSnapshot(ctx, a), a)
}

type credentialBrokerSnapshotKey struct{}
type credentialBrokerSnapshot struct {
	id   int64
	auth map[string]any
}

func withCredentialBrokerSnapshot(ctx context.Context, a *Account) context.Context {
	return context.WithValue(ctx, credentialBrokerSnapshotKey{}, credentialBrokerSnapshot{id: a.ID, auth: CredentialBrokerAuthSnapshot(a.Credentials)})
}

// Native refresh performs additional owner reads. Do not consume a newly
// reauthorized RT and then try to persist it against an older CAS snapshot.
func requireCredentialBrokerSnapshot(ctx context.Context, a *Account) error {
	expected, ok := ctx.Value(credentialBrokerSnapshotKey{}).(credentialBrokerSnapshot)
	if !ok {
		return nil
	}
	if !brokerEligible(a) || a.ID != expected.id || !reflect.DeepEqual(expected.auth, CredentialBrokerAuthSnapshot(a.Credentials)) {
		return ErrCredentialBrokerStateChanged
	}
	return nil
}

type credentialBrokerRefreshKey struct{}

func withCredentialBrokerRefresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, credentialBrokerRefreshKey{}, true)
}
func isCredentialBrokerRefresh(ctx context.Context) bool {
	v, _ := ctx.Value(credentialBrokerRefreshKey{}).(bool)
	return v
}

var ErrCredentialBrokerStateChanged = errors.New("credential broker account state changed")

type credentialBrokerCredentialsUpdater interface {
	UpdateOpenAIBrokerCredentialsIfUnchanged(context.Context, int64, map[string]any, map[string]any) (bool, error)
}

var brokerAuthFields = []string{"access_token", "refresh_token", "id_token", "client_id", "chatgpt_account_id", "chatgpt_user_id", "organization_id", "auth_mode", "openai_auth_mode"}

func CredentialBrokerAuthSnapshot(creds map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range brokerAuthFields {
		out[k] = creds[k]
	}
	return out
}
func CredentialBrokerAuthUpdate(creds map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range append(append([]string{}, brokerAuthFields...), "expires_at", "email", "plan_type", "subscription_expires_at", "_token_version", "token_type", "chatgpt_account_is_fedramp") {
		if v, ok := creds[k]; ok {
			out[k] = v
		}
	}
	return out
}
