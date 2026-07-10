package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a Redis-backed implementation of FixedWindowStore, SlidingWindowStore,
// and TokenBucketStore. Each algorithm's logic runs inside a Lua script executed
// atomically on the Redis server to prevent race conditions across nodes.
type RedisStore struct {
	client                *redis.Client
	fixedWindowScript     *redis.Script
	slidingWindowScript   *redis.Script
	tokenBucketScript     *redis.Script
	fixedWindowSyncScript *redis.Script
	tokenBucketSyncScript *redis.Script
}

// NewRedisStore returns a RedisStore backed by the given Redis client.
// fwScript, swScript, and tbScript are the Lua source strings for the fixed window,
// sliding window, and token bucket algorithms respectively. fwSyncScript and
// tbSyncScript are the Lua source strings for the batched delta-sync variants
// of the fixed window and token bucket algorithms, used by node-local caching.
func NewRedisStore(redisClient *redis.Client, fwScript string, swScript string, tbScript string, fwSyncScript string, tbSyncScript string) *RedisStore {
	newRedisStore := RedisStore{
		client:                redisClient,
		fixedWindowScript:     redis.NewScript(fwScript),
		slidingWindowScript:   redis.NewScript(swScript),
		tokenBucketScript:     redis.NewScript(tbScript),
		fixedWindowSyncScript: redis.NewScript(fwSyncScript),
		tokenBucketSyncScript: redis.NewScript(tbSyncScript),
	}
	return &newRedisStore
}

// AllowFixedWindow increments the request counter for key within the current window
// and returns true if the count is within limit, false if it should be throttled.
// The window resets automatically when the Redis key expires.
func (store *RedisStore) AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	cmd := store.fixedWindowScript.Run(ctx, store.client, []string{key}, limit, window.Milliseconds())

	res, err := cmd.Int()
	if err != nil {
		return false, fmt.Errorf("failed to execute fixed window algorithm: %w", err)
	}
	return res == 1, nil
}

// AllowSlidingWindow records the current request for key in a sliding window and
// returns true if the number of requests within the rolling window is within limit,
// false if it should be throttled.
func (store *RedisStore) AllowSlidingWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	cmd := store.slidingWindowScript.Run(ctx, store.client, []string{key}, limit, window.Milliseconds())

	res, err := cmd.Int()
	if err != nil {
		return false, fmt.Errorf("failed to execute sliding window algorithm: %w", err)
	}
	return res == 1, nil
}

// AllowTokenBucket attempts to consume one token from the bucket for key, refilling
// based on elapsed time and rate. Returns true if a token was consumed, false if the
// bucket is empty and the request should be throttled.
func (store *RedisStore) AllowTokenBucket(ctx context.Context, key string, capacity float64, rate float64) (bool, error) {
	cmd := store.tokenBucketScript.Run(ctx, store.client, []string{key}, capacity, rate)

	res, err := cmd.Int()
	if err != nil {
		return false, fmt.Errorf("failed to execute token bucket algorithm: %w", err)
	}
	return res == 1, nil
}

// SyncFixedWindow folds delta (requests already admitted locally since the last
// sync) into the authoritative counter for key and returns the resulting total.
func (store *RedisStore) SyncFixedWindow(ctx context.Context, key string, delta int64, window time.Duration) (int64, error) {
	cmd := store.fixedWindowSyncScript.Run(ctx, store.client, []string{key}, delta, window.Milliseconds())

	total, err := cmd.Int64()
	if err != nil {
		return 0, fmt.Errorf("failed to execute fixed window sync: %w", err)
	}
	return total, nil
}

// SyncTokenBucket refills the bucket for key based on elapsed time, then subtracts
// delta (tokens already consumed locally since the last sync), and returns the
// resulting token count. The result can be negative, signaling local over-admission
// during the interval since the last sync.
func (store *RedisStore) SyncTokenBucket(ctx context.Context, key string, delta float64, capacity float64, rate float64) (float64, error) {
	cmd := store.tokenBucketSyncScript.Run(ctx, store.client, []string{key}, capacity, rate, delta)

	remaining, err := cmd.Float64()
	if err != nil {
		return 0, fmt.Errorf("failed to execute token bucket sync: %w", err)
	}
	return remaining, nil
}
