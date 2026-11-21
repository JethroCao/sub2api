package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	oauthTokenKeyPrefix       = "oauth:token:"
	oauthRefreshLockKeyPrefix = "oauth:refresh_lock:"
)

type geminiTokenCache struct {
	rdb *redis.Client
}

func NewGeminiTokenCache(rdb *redis.Client) service.GeminiTokenCache {
	return &geminiTokenCache{rdb: rdb}
}

func (c *geminiTokenCache) GetAccessToken(ctx context.Context, cacheKey string) (string, error) {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Get(ctx, key).Result()
}

func (c *geminiTokenCache) SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Set(ctx, key, token, ttl).Err()
}

func (c *geminiTokenCache) DeleteAccessToken(ctx context.Context, cacheKey string) error {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Del(ctx, key).Err()
}

func (c *geminiTokenCache) AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (bool, error) {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return c.rdb.SetNX(ctx, key, 1, ttl).Result()
}

func (c *geminiTokenCache) ReleaseRefreshLock(ctx context.Context, cacheKey string) error {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	// A legacy caller must never delete an owned replacement lease.
	return releaseOwnedOAuthLockScript.Run(ctx, c.rdb, []string{key}, "1").Err()
}

var releaseOwnedOAuthLockScript = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`)
var extendOwnedOAuthLockScript = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE',KEYS[1],ARGV[2]) end return 0`)

func (c *geminiTokenCache) AcquireOwnedRefreshLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	owner := uuid.NewString()
	ok, err := c.rdb.SetNX(ctx, oauthRefreshLockKeyPrefix+key, owner, ttl).Result()
	return owner, ok, err
}
func (c *geminiTokenCache) ExtendOwnedRefreshLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	n, err := extendOwnedOAuthLockScript.Run(ctx, c.rdb, []string{oauthRefreshLockKeyPrefix + key}, owner, ttl.Milliseconds()).Int()
	return n == 1, err
}
func (c *geminiTokenCache) ReleaseOwnedRefreshLock(ctx context.Context, key, owner string) error {
	return releaseOwnedOAuthLockScript.Run(ctx, c.rdb, []string{oauthRefreshLockKeyPrefix + key}, owner).Err()
}
