package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var errOAuthRefreshLeaseLost = errors.New("oauth refresh lease unavailable")

type oauthRefreshLeaseKey struct{}
type oauthRefreshLease struct {
	cache      OwnedOAuthRefreshLockCache
	key, owner string
	ttl        time.Duration
}

func (l oauthRefreshLease) extend(parent context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
	defer cancel()
	ok, err := l.cache.ExtendOwnedRefreshLock(ctx, l.key, l.owner, l.ttl)
	if err != nil || !ok {
		return errOAuthRefreshLeaseLost
	}
	return nil
}

func confirmOAuthRefreshLease(ctx context.Context) error {
	if lease, ok := ctx.Value(oauthRefreshLeaseKey{}).(oauthRefreshLease); ok {
		return lease.extend(ctx)
	}
	return nil
}

func (api *OAuthRefreshAPI) maintainOwnedRefreshLease(parent context.Context, cache OwnedOAuthRefreshLockCache, key, owner string) (context.Context, func()) {
	lease := oauthRefreshLease{cache: cache, key: key, owner: owner, ttl: api.lockTTL}
	ctx, cancel := context.WithCancelCause(context.WithValue(parent, oauthRefreshLeaseKey{}, lease))
	stop, done := make(chan struct{}), make(chan struct{})
	interval := api.lockTTL / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := lease.extend(ctx); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(stop)
		<-done
		cancel(context.Canceled)
		cleanup, finish := context.WithTimeout(context.WithoutCancel(parent), defaultRefreshLockReleaseTimeout)
		defer finish()
		if err := cache.ReleaseOwnedRefreshLock(cleanup, key, owner); err != nil {
			slog.Warn("oauth_refresh_owned_lock_release_failed", "cache_key", key)
		}
	}
}
