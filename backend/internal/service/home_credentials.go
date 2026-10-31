package service

import (
	"context"
	"errors"
	"strconv"
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
type homeRecoveryAttempt struct {
	until   time.Time
	pending bool
}
type homeCredentialGeneration struct {
	sequence uint64
	observed string // active recovery comparison; cleared when a valid token publishes
	rejected string // latest known rejection; retained to fence delayed admission
}

// HomeCredentialsService owns only transient ATs. It never stores or receives RTs.
// Auxiliary Resolve uses Home Refresh because Home exposes no read-only credential API.
// Actual model sends must independently Acquire even when Resolve has a warm cache.
type HomeCredentialsService struct {
	recoveries  map[string]homeRecoveryAttempt
	failures    map[string]homeCachedFailure
	close       func() error
	accounts    AccountRepository
	client      homeCredentialClient
	mu          sync.Mutex
	cache       map[string]homeCachedCredential
	generations map[string]homeCredentialGeneration
	sequence    uint64
	flight      singleflight.Group
}

func NewHomeCredentialsService(accounts AccountRepository, client homeCredentialClient) *HomeCredentialsService {
	return &HomeCredentialsService{accounts: accounts, client: client, cache: make(map[string]homeCachedCredential), failures: make(map[string]homeCachedFailure), recoveries: make(map[string]homeRecoveryAttempt), generations: make(map[string]homeCredentialGeneration)}
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
	// Native-only callers include independent plugins. Every ownership reread
	// must retain that constraint, including reads made inside Home resolution.
	if err := requireNativeCredentialMutation(ctx, a); err != nil {
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
	return s.resolve(ctx, a, "", false, false)
}
func (s *HomeCredentialsService) Refresh(ctx context.Context, a *Account, observedAT string) (homeclient.Credential, error) {
	return s.resolve(ctx, a, observedAT, observedAT != "", false)
}
func (s *HomeCredentialsService) resolve(ctx context.Context, a *Account, observed string, force, awaitRefresh bool) (credential homeclient.Credential, err error) {
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
	if force && a.HasHomeBinding() {
		expectedID, bindingErr := a.HomeCredentialID()
		if bindingErr != nil {
			return zero, bindingErr
		}
		if expectedID != id {
			return zero, ErrHomeOwnershipChanged
		}
	}
	if s.client == nil || !s.client.Ready() {
		return zero, ErrHomeUnavailable
	}
	s.mu.Lock()
	cached, ok := s.cache[id]
	if ok && time.Now().Before(cached.until) && (!force || (observed != "" && cached.credential.AccessToken != observed)) {
		s.mu.Unlock()
		return cached.credential, nil
	}
	generation := s.generations[id]
	if generation.sequence == 0 || (force && observed != "" && generation.observed != observed) {
		generation = s.advanceGenerationLocked(id, observed)
	}
	// A cache miss racing a recovery must carry the same observed rejection.
	if generation.observed != "" {
		observed = generation.observed
		force = true
	}
	s.mu.Unlock()
	ch := s.flight.DoChan(id+":"+strconv.FormatUint(generation.sequence, 10), func() (any, error) {
		s.mu.Lock()
		if s.generations[id].sequence != generation.sequence {
			latest, valid := s.cache[id]
			s.mu.Unlock()
			if valid && time.Now().Before(latest.until) {
				return latest.credential, nil
			}
			return zero, ErrHomeUnavailable
		}
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
		return s.publishResolvedCredential(id, generation, c, err)
	})
	if awaitRefresh {
		// Recovery ownership lasts until the actual shared worker has finished,
		// including its conditional cache publication after cancellation.
		result := <-ch
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(homeclient.Credential), nil
	}
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
	snapshot := ApplyHomeCredential(a, owner, c)
	// Resolve rereads ownership. Keep recovery bound to the credential actually
	// returned if an administrator rebound the row between these two reads.
	snapshot.Extra[HomeCredentialIDKey] = c.ID
	return snapshot, nil
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
	s.mu.Lock()
	generation := s.generations[id]
	if generation.sequence == 0 {
		generation = s.advanceGenerationLocked(id, "")
	}
	s.mu.Unlock()
	request.CredentialID = id
	lease, err := s.client.Acquire(ctx, request)
	if err != nil {
		return nil, err
	}
	if err = s.cacheAcquiredCredential(id, generation, lease.Credential); err != nil {
		result := homeclient.Result{Failed: true, ErrorCode: "home_invalid_credential"}
		if errors.Is(err, ErrHomeUnavailable) {
			// No model bytes were sent. Release superseded valid admission as a
			// local cancellation, never as a provider authentication failure.
			result.StatusCode, result.ErrorCode = 499, "home_credential_superseded"
		}
		lease.Finish(result)
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
	return s.cacheAcquiredCredential(id, homeCredentialGeneration{}, c)
}

func (s *HomeCredentialsService) cacheAcquiredCredential(id string, generation homeCredentialGeneration, c homeclient.Credential) error {
	if err := validateHomeCredential(id, c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.generations[id]
	if current.rejected != "" && c.AccessToken == current.rejected {
		return ErrHomeUnavailable
	}
	if generation.sequence != 0 && current.sequence != generation.sequence {
		if latest, ok := s.cache[id]; ok && time.Now().Before(latest.until) && latest.credential.AccessToken == c.AccessToken {
			return nil
		}
		return ErrHomeUnavailable
	}
	s.advanceGenerationLocked(id, "")
	s.cacheCredentialLocked(id, c)
	return nil
}

func validateHomeCredential(id string, c homeclient.Credential) error {
	now := time.Now()
	if c.ID != id || c.AccessToken == "" || c.AccountID == "" || (!c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now)) {
		return ErrHomeBinding
	}
	return nil
}

// A late refresh may neither replace a newer admission credential nor erase it
// on failure. A rejection advances the same fence even when no AT was cached.
func (s *HomeCredentialsService) publishResolvedCredential(id string, generation homeCredentialGeneration, c homeclient.Credential, err error) (homeclient.Credential, error) {
	if err == nil {
		err = validateHomeCredential(id, c)
		if err == nil && generation.rejected != "" && c.AccessToken == generation.rejected {
			err = ErrHomeUnavailable
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generations[id].sequence != generation.sequence {
		if latest, ok := s.cache[id]; ok && time.Now().Before(latest.until) {
			return latest.credential, nil
		}
		return homeclient.Credential{}, ErrHomeUnavailable
	}
	// Completed comparisons must not pin a later ordinary expiry to an old
	// observation, including a comparison that failed validation. Retain the
	// rejected AT as a publication fence while failure backoff remains active.
	generation.observed = ""
	s.generations[id] = generation
	if err != nil {
		delete(s.cache, id)
		s.cacheFailureLocked(id, err, time.Now().Add(5*time.Second))
		return homeclient.Credential{}, err
	}
	s.cacheCredentialLocked(id, c)
	return c, nil
}

// Caller holds mu. Global sequence numbers avoid ABA after bounded-map eviction.
func (s *HomeCredentialsService) advanceGenerationLocked(id, observed string) homeCredentialGeneration {
	rejected := observed
	if rejected == "" {
		rejected = s.generations[id].rejected
	}
	if len(s.generations) >= 2048 {
		for key := range s.generations {
			if key != id && !s.recoveries[key].pending {
				delete(s.generations, key)
				delete(s.cache, key)
				delete(s.failures, key)
				delete(s.recoveries, key)
				break
			}
		}
	}
	s.sequence++
	generation := homeCredentialGeneration{sequence: s.sequence, observed: observed, rejected: rejected}
	s.generations[id] = generation
	return generation
}

func (s *HomeCredentialsService) cacheCredentialLocked(id string, c homeclient.Credential) {
	now := time.Now()
	// Home ATs stay cached until their advertised expiry; only unknown expiry uses
	// a bounded fallback. Every model attempt still independently calls Acquire.
	until := now.Add(5 * time.Minute)
	if !c.ExpiresAt.IsZero() {
		until = c.ExpiresAt.Add(-30 * time.Second)
		if until.Before(now) {
			until = c.ExpiresAt
		}
	}
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
}

// Caller holds mu. A failed/rejected credential must not immediately turn into
// an unbounded cache-miss refresh loop.
func (s *HomeCredentialsService) cacheFailureLocked(id string, err error, until time.Time) {
	if len(s.failures) >= 1024 {
		for key := range s.failures {
			delete(s.failures, key)
			break
		}
	}
	s.failures[id] = homeCachedFailure{err, until}
}

// Recover reports an actual upstream 401 to Home's refresh path. Some legacy
// callers only retain the persisted account (no AT); the empty observed value
// remains honest and recovery is bounded even when refresh succeeds repeatedly.
func (s *HomeCredentialsService) Recover(ctx context.Context, a *Account, observedAT string) error {
	if s == nil {
		return ErrHomeUnavailable
	}
	// A 401 remains an observed rejection when its request is canceled. Bound
	// the authoritative owner read independently; never refresh an unverified
	// binding, and keep provider recovery detached below.
	ownerCtx, cancelOwner := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	owner, err := s.CurrentOwner(ownerCtx, a)
	cancelOwner()
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
	if a.HasHomeBinding() {
		expectedID, bindingErr := a.HomeCredentialID()
		if bindingErr != nil {
			return bindingErr
		}
		if expectedID != id {
			return ErrHomeOwnershipChanged
		}
	}
	now := time.Now()
	s.mu.Lock()
	previous := s.recoveries[id]
	if previous.pending || now.Before(previous.until) {
		// A newly refreshed token may itself receive 401 during the cooldown.
		// Fence that token without another rotation. Repeated rejections of the
		// original token keep its in-progress recovery generation intact.
		if cached, ok := s.cache[id]; observedAT != "" && (!ok || cached.credential.AccessToken == observedAT) && s.generations[id].rejected != observedAT {
			delete(s.cache, id)
			s.advanceGenerationLocked(id, observedAT)
			s.cacheFailureLocked(id, ErrHomeUnavailable, previous.until)
		}
		s.mu.Unlock()
		return nil
	}
	if len(s.recoveries) >= 1024 {
		for key, attempt := range s.recoveries {
			if !attempt.pending && !now.Before(attempt.until) {
				delete(s.recoveries, key)
			}
		}
		if len(s.recoveries) >= 1024 {
			for key, attempt := range s.recoveries {
				if !attempt.pending {
					delete(s.recoveries, key)
					break
				}
			}
		}
	}
	// The actual upstream rejection invalidates this AT even if Home is offline
	// or the caller is canceled before the bounded refresh can complete.
	if cached, ok := s.cache[id]; observedAT == "" || !ok || cached.credential.AccessToken == observedAT {
		delete(s.cache, id)
		s.advanceGenerationLocked(id, observedAT)
	}
	s.recoveries[id] = homeRecoveryAttempt{until: now.Add(5 * time.Second), pending: true}
	s.mu.Unlock()
	// Cancellation releases the caller, not ownership of the bounded recovery.
	// Keep pending set until the detached worker actually terminates.
	done := make(chan error, 1)
	go func() {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, err := s.resolve(callCtx, owner, observedAT, true, true)
		s.mu.Lock()
		if attempt, ok := s.recoveries[id]; ok {
			attempt.pending = false
			s.recoveries[id] = attempt
		}
		s.mu.Unlock()
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}
