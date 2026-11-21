package service

import (
	"context"
	"time"
)

// GeminiTokenCache stores short-lived access tokens and coordinates refresh to avoid stampedes.
type GeminiTokenCache interface {
	// cacheKey should be stable for the token scope; for GeminiCli OAuth we primarily use project_id.
	GetAccessToken(ctx context.Context, cacheKey string) (string, error)
	SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error
	DeleteAccessToken(ctx context.Context, cacheKey string) error

	AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (bool, error)
	ReleaseRefreshLock(ctx context.Context, cacheKey string) error
}

// OwnedOAuthRefreshLockCache prevents expired owners from deleting replacement
// leases. Production OAuth refreshes renew these leases while work is in flight.
type OwnedOAuthRefreshLockCache interface {
	AcquireOwnedRefreshLock(context.Context, string, time.Duration) (string, bool, error)
	ExtendOwnedRefreshLock(context.Context, string, string, time.Duration) (bool, error)
	ReleaseOwnedRefreshLock(context.Context, string, string) error
}
