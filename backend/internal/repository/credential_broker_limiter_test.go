package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCredentialBrokerRedisLimitsSharedAcrossNodes(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	a, b := NewCredentialBrokerLimiter(rdb), NewCredentialBrokerLimiter(rdb)
	ctx := context.Background()
	ok, _, err := a.Allow(ctx, "forced:4", 1, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	ok, retry, err := b.Allow(ctx, "forced:4", 1, 30*time.Second)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 30*time.Second, retry)
	ok, _, err = b.Allow(ctx, "forced:14", 1, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	m.FastForward(31 * time.Second)
	ok, _, err = b.Allow(ctx, "forced:4", 1, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	for i := 0; i < 60; i++ {
		ok, _, err = a.Allow(ctx, "requests:node", 60, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, _, err = b.Allow(ctx, "requests:node", 60, time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	m.Close()
	_, _, err = b.Allow(ctx, "requests:node", 60, time.Minute)
	require.Error(t, err)
}

func TestOAuthRefreshOwnedLeaseDoesNotDeleteReplacement(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := NewGeminiTokenCache(rdb).(*geminiTokenCache)
	ctx := context.Background()
	old, ok, err := cache.AcquireOwnedRefreshLock(ctx, "openai:4", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	m.FastForward(61 * time.Second)
	current, ok, err := cache.AcquireOwnedRefreshLock(ctx, "openai:4", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, old, current)
	require.NoError(t, cache.ReleaseOwnedRefreshLock(ctx, "openai:4", old))
	ok, err = cache.ExtendOwnedRefreshLock(ctx, "openai:4", current, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.ExtendOwnedRefreshLock(ctx, "openai:4", old, time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
}
