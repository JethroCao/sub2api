package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"golang.org/x/sync/singleflight"
)

var ErrHomeUnavailable = errors.New("Home credential service is unavailable")

type homeCredentialClient interface {
	Ready() bool
	Refresh(context.Context, string, string) (homeclient.Credential, error)
	Acquire(context.Context, homeclient.Request) (*homeclient.Lease, error)
}
type homeCachedFailure struct {
	err   error
	until time.Time
}
type homeCachedCredential struct {
	credential homeclient.Credential
	until      time.Time
}

// HomeCredentialsService owns only transient ATs. It never stores or receives RTs.
// Auxiliary Resolve uses Home Refresh because Home exposes no read-only credential API.
// Actual model sends must independently Acquire even when Resolve has a warm cache.
type HomeCredentialsService struct {
	recoveries map[string]time.Time
	failures   map[string]homeCachedFailure
	close      func() error
	accounts   AccountRepository
	client     homeCredentialClient
	mu         sync.Mutex
	cache      map[string]homeCachedCredential
	flight     singleflight.Group
}

func NewHomeCredentialsService(accounts AccountRepository, client homeCredentialClient) *HomeCredentialsService {
	return &HomeCredentialsService{accounts: accounts, client: client, cache: make(map[string]homeCachedCredential), failures: make(map[string]homeCachedFailure), recoveries: make(map[string]time.Time)}
}
func currentHomeCredentialOwner(ctx context.Context, repo AccountRepository, a *Account) (*Account, error) {
	if a == nil {
		return nil, ErrHomeBinding
	}
	if repo != nil && a.ID > 0 {
		var err error
		a, err = repo.GetByID(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if a == nil {
			return nil, ErrAccountNotFound
		}
	}
	if a.IsCredentialShadow() {
		if a.HasHomeBinding() {
			return nil, ErrHomeBinding
		}
		var err error
		a, err = resolveCredentialAccount(ctx, repo, a)
		if err != nil {
			return nil, err
		}
	}
	if _, err := a.HomeCredentialID(); err != nil {
		return nil, err
	}
	return a, nil
}
func (s *HomeCredentialsService) CurrentOwner(ctx context.Context, a *Account) (*Account, error) {
	if s == nil {
		if a.HasHomeBinding() {
			return nil, ErrHomeUnavailable
		}
		return a, nil
	}
	return currentHomeCredentialOwner(ctx, s.accounts, a)
}
func (s *HomeCredentialsService) Resolve(ctx context.Context, a *Account) (homeclient.Credential, error) {
	return s.resolve(ctx, a, "", false)
}
func (s *HomeCredentialsService) Refresh(ctx context.Context, a *Account, observedAT string) (homeclient.Credential, error) {
	return s.resolve(ctx, a, observedAT, observedAT != "")
}
func (s *HomeCredentialsService) resolve(ctx context.Context, a *Account, observed string, force bool) (credential homeclient.Credential, err error) {
	defer func() { err = homeControlError(err) }()
	var zero homeclient.Credential
	if s == nil {
		return zero, ErrHomeUnavailable
	}
	owner, err := s.CurrentOwner(ctx, a)
	if err != nil {
		return zero, err
	}
	id, err := owner.HomeCredentialID()
	if err != nil {
		return zero, err
	}
	if id == "" {
		return zero, ErrHomeBinding
	}
	if s.client == nil || !s.client.Ready() {
		return zero, ErrHomeUnavailable
	}
	s.mu.Lock()
	cached, ok := s.cache[id]
	s.mu.Unlock()
	if ok && time.Now().Before(cached.until) && (!force || (observed != "" && cached.credential.AccessToken != observed)) {
		return cached.credential, nil
	}
	ch := s.flight.DoChan(id, func() (any, error) {
		s.mu.Lock()
		failure, failed := s.failures[id]
		s.mu.Unlock()
		if failed && time.Now().Before(failure.until) {
			return zero, failure.err
		}

		s.mu.Lock()
		latest, valid := s.cache[id]
		s.mu.Unlock()
		if valid && time.Now().Before(latest.until) && (!force || (observed != "" && latest.credential.AccessToken != observed)) {
			return latest.credential, nil
		}
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		// An empty observed token is an honest cache miss, never a made-up hash.
		c, err := s.client.Refresh(callCtx, id, observed)
		if err == nil {
			err = s.cacheCredential(id, c)
		}
		if err != nil {
			s.mu.Lock()
			delete(s.cache, id)
			if len(s.failures) >= 1024 {
				for key := range s.failures {
					delete(s.failures, key)
					break
				}
			}
			s.failures[id] = homeCachedFailure{err, time.Now().Add(5 * time.Second)}
			s.mu.Unlock()
			return zero, err
		}
		return c, nil
	})
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(homeclient.Credential), nil
	}
}

// Prepare overlays an immutable request-local copy, preserving local routing/billing ID.
func (s *HomeCredentialsService) Prepare(ctx context.Context, a *Account) (*Account, error) {
	owner, err := s.CurrentOwner(ctx, a)
	if err != nil {
		return nil, err
	}
	if !owner.HasHomeBinding() {
		return a, nil
	}
	c, err := s.Resolve(ctx, owner)
	if err != nil {
		return nil, err
	}
	return ApplyHomeCredential(a, owner, c), nil
}
func ApplyHomeCredential(a, owner *Account, c homeclient.Credential) *Account {
	copy := *a
	copy.Credentials = SanitizeHomeCredentials(a.Credentials)
	copy.Credentials["access_token"] = c.AccessToken
	copy.Credentials["chatgpt_account_id"] = c.AccountID
	if !c.ExpiresAt.IsZero() {
		copy.Credentials["expires_at"] = c.ExpiresAt.Format(time.RFC3339)
	}
	copy.Extra = shallowCopyMap(a.Extra)
	if copy.Extra == nil {
		copy.Extra = map[string]any{}
	}
	for k, v := range homeBindingValues(owner.Extra) {
		copy.Extra[k] = v
	}
	return &copy
}
func (s *HomeCredentialsService) Acquire(ctx context.Context, a *Account, request homeclient.Request) (*homeclient.Lease, error) {
	if s == nil || s.client == nil {
		return nil, ErrHomeUnavailable
	}
	owner, err := s.CurrentOwner(ctx, a)
	if err != nil {
		return nil, err
	}
	id, err := owner.HomeCredentialID()
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrHomeBinding
	}
	request.CredentialID = id
	lease, err := s.client.Acquire(ctx, request)
	if err != nil {
		return nil, err
	}
	if err = s.cacheCredential(id, lease.Credential); err != nil {
		lease.Finish(homeclient.Result{Failed: true, ErrorCode: "home_invalid_credential"})
		return nil, err
	}
	return lease, nil
}

func prepareHomeAccount(ctx context.Context, repo AccountRepository, home *HomeCredentialsService, a *Account) (*Account, error) {
	owner, err := currentHomeCredentialOwner(ctx, repo, a)
	if err != nil {
		return nil, err
	}
	if !owner.HasHomeBinding() {
		return a, nil
	}
	return home.Prepare(ctx, a)
}

func (s *HomeCredentialsService) Close() error {
	if s != nil && s.close != nil {
		return s.close()
	}
	return nil
}

func (s *HomeCredentialsService) cacheCredential(id string, c homeclient.Credential) error {
	now := time.Now()
	if c.ID != id || c.AccessToken == "" || c.AccountID == "" || (!c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now)) {
		return ErrHomeBinding
	}
	// Home ATs stay cached until their advertised expiry; only unknown expiry uses
	// a bounded fallback. Every model attempt still independently calls Acquire.
	until := now.Add(5 * time.Minute)
	if !c.ExpiresAt.IsZero() {
		until = c.ExpiresAt.Add(-30 * time.Second)
		if until.Before(now) {
			until = c.ExpiresAt
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= 1024 {
		for key, value := range s.cache {
			if !now.Before(value.until) {
				delete(s.cache, key)
			}
		}
		if len(s.cache) >= 1024 {
			for key := range s.cache {
				delete(s.cache, key)
				break
			}
		}
	}
	if len(s.failures) >= 1024 {
		for key := range s.failures {
			delete(s.failures, key)
		}
	}
	s.cache[id] = homeCachedCredential{c, until}
	delete(s.failures, id)
	return nil
}

// Recover reports an actual upstream 401 to Home's refresh path. Some legacy
// callers only retain the persisted account (no AT); the empty observed value
// remains honest and recovery is bounded even when refresh succeeds repeatedly.
func (s *HomeCredentialsService) Recover(ctx context.Context, a *Account, observedAT string) error {
	if s == nil {
		return ErrHomeUnavailable
	}
	owner, err := s.CurrentOwner(ctx, a)
	if err != nil {
		return err
	}
	id, err := owner.HomeCredentialID()
	if err != nil {
		return err
	}
	if id == "" {
		return ErrHomeBinding
	}
	now := time.Now()
	s.mu.Lock()
	if now.Before(s.recoveries[id]) {
		s.mu.Unlock()
		return nil
	}
	if len(s.recoveries) >= 1024 {
		for key, until := range s.recoveries {
			if !now.Before(until) {
				delete(s.recoveries, key)
			}
		}
		if len(s.recoveries) >= 1024 {
			for key := range s.recoveries {
				delete(s.recoveries, key)
				break
			}
		}
	}
	s.recoveries[id] = now.Add(5 * time.Second)
	s.mu.Unlock()
	_, err = s.resolve(ctx, owner, observedAT, true)
	return err
}
