package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type redisBrokerLimiter struct{ client *redis.Client }

func NewCredentialBrokerLimiter(client *redis.Client) service.CredentialBrokerLimiter {
	return &redisBrokerLimiter{client: client}
}

var brokerLimitScript = redis.NewScript(`
local n=redis.call('INCR',KEYS[1])
local ttl=redis.call('PTTL',KEYS[1])
if n==1 or ttl<0 then redis.call('PEXPIRE',KEYS[1],ARGV[1]); ttl=tonumber(ARGV[1]) end
local allowed=0
if n<=tonumber(ARGV[2]) then allowed=1 end
return {allowed,ttl}
`)

func (l *redisBrokerLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if l == nil || l.client == nil {
		return false, 0, errors.New("broker Redis unavailable")
	}
	values, err := brokerLimitScript.Run(ctx, l.client, []string{"credential_broker:" + key}, window.Milliseconds(), limit).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	if len(values) != 2 {
		return false, 0, errors.New("invalid broker limiter result")
	}
	return values[0] == 1, time.Duration(values[1]) * time.Millisecond, nil
}
