package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a Redis-backed implementation of FixedWindowStore, SlidingWindowStore,
// TokenBucketStore, and LeakyBucketStore. Each algorithm's logic runs inside a Lua
// script executed atomically on the Redis server to prevent race conditions across nodes.
//
// The store only ever needs to run scripts, so it holds a redis.Scripter rather
// than a concrete client: a standalone *redis.Client, a *redis.ClusterClient,
// and a *redis.Ring all satisfy it. Every script touches exactly KEYS[1], which
// is what Redis Cluster requires of a script (all keys in one hash slot), so
// the same Lua runs unmodified against a cluster. See the key-layout note at
// the top of each script under lua/.
type RedisStore struct {
	client                redis.Scripter
	fixedWindowScript     *redis.Script
	slidingWindowScript   *redis.Script
	tokenBucketScript     *redis.Script
	leakyBucketScript     *redis.Script
	fixedWindowSyncScript *redis.Script
	tokenBucketSyncScript *redis.Script
	recorder              LatencyRecorder
}

// NewRedisStore returns a RedisStore that runs its scripts on redisClient, which
// may be any go-redis client type that implements redis.Scripter (standalone,
// cluster, or ring).
// fwScript, swScript, tbScript, and lbScript are the Lua source strings for the fixed
// window, sliding window, token bucket, and leaky bucket algorithms respectively.
// fwSyncScript and tbSyncScript are the Lua source strings for the batched delta-sync
// variants of the fixed window and token bucket algorithms, used by node-local caching.
// recorder observes the round-trip latency of every script execution, labeled by algorithm.
func NewRedisStore(redisClient redis.Scripter, fwScript string, swScript string, tbScript string, lbScript string, fwSyncScript string, tbSyncScript string, recorder LatencyRecorder) *RedisStore {
	newRedisStore := RedisStore{
		client:                redisClient,
		fixedWindowScript:     redis.NewScript(fwScript),
		slidingWindowScript:   redis.NewScript(swScript),
		tokenBucketScript:     redis.NewScript(tbScript),
		leakyBucketScript:     redis.NewScript(lbScript),
		fixedWindowSyncScript: redis.NewScript(fwSyncScript),
		tokenBucketSyncScript: redis.NewScript(tbSyncScript),
		recorder:              recorder,
	}
	return &newRedisStore
}

// AllowFixedWindow increments the request counter for key within the current window
// and returns true if the count is within limit, false if it should be throttled.
// The window resets automatically when the Redis key expires.
func (store *RedisStore) AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	start := time.Now()
	cmd := store.fixedWindowScript.Run(ctx, store.client, []string{key}, limit, window.Milliseconds())
	store.recorder.ObserveRedisLatency("fixed_window", time.Since(start))

	res, err := cmd.Int64Slice()
	if err != nil {
		return false, fmt.Errorf("failed to execute fixed window algorithm: %w", err)
	}
	store.recorder.ObserveLuaExecution("fixed_window", time.Duration(res[1])*time.Microsecond)
	return res[0] == 1, nil
}

// AllowSlidingWindow records the current request for key in a sliding window and
// returns true if the number of requests within the rolling window is within limit,
// false if it should be throttled.
func (store *RedisStore) AllowSlidingWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	start := time.Now()
	cmd := store.slidingWindowScript.Run(ctx, store.client, []string{key}, limit, window.Milliseconds())
	store.recorder.ObserveRedisLatency("sliding_window", time.Since(start))

	res, err := cmd.Int64Slice()
	if err != nil {
		return false, fmt.Errorf("failed to execute sliding window algorithm: %w", err)
	}
	store.recorder.ObserveLuaExecution("sliding_window", time.Duration(res[1])*time.Microsecond)
	return res[0] == 1, nil
}

// AllowTokenBucket attempts to consume one token from the bucket for key, refilling
// based on elapsed time and rate. Returns true if a token was consumed, false if the
// bucket is empty and the request should be throttled.
func (store *RedisStore) AllowTokenBucket(ctx context.Context, key string, capacity float64, rate float64) (bool, error) {
	start := time.Now()
	cmd := store.tokenBucketScript.Run(ctx, store.client, []string{key}, capacity, rate)
	store.recorder.ObserveRedisLatency("token_bucket", time.Since(start))

	res, err := cmd.Int64Slice()
	if err != nil {
		return false, fmt.Errorf("failed to execute token bucket algorithm: %w", err)
	}
	store.recorder.ObserveLuaExecution("token_bucket", time.Duration(res[1])*time.Microsecond)
	return res[0] == 1, nil
}

// AllowLeakyBucket admits one request against key's GCRA schedule, returning true
// if the request arrived on schedule, false if it arrived too early and should be
// throttled. limit and window define the emission interval (window / limit); burst
// defines the tolerance for clumped arrivals (emission_interval * (burst - 1)).
// burst values less than 1 are treated as 1 (no clumping tolerance). When denied,
// retryAfter is the exact duration until key's schedule will next admit a request.
func (store *RedisStore) AllowLeakyBucket(ctx context.Context, key string, limit int64, window time.Duration, burst int64) (allowed bool, retryAfter time.Duration, err error) {
	if burst < 1 {
		burst = 1
	}
	emissionIntervalMs := float64(window.Milliseconds()) / float64(limit)
	toleranceMs := emissionIntervalMs * float64(burst-1)

	start := time.Now()
	cmd := store.leakyBucketScript.Run(ctx, store.client, []string{key}, emissionIntervalMs, toleranceMs)
	store.recorder.ObserveRedisLatency("leaky_bucket", time.Since(start))

	res, err := cmd.Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("failed to execute leaky bucket algorithm: %w", err)
	}
	store.recorder.ObserveLuaExecution("leaky_bucket", time.Duration(res[2])*time.Microsecond)
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond, nil
}

// SyncFixedWindow folds delta (requests already admitted locally since the last
// sync) into the authoritative counter for key and returns the resulting total.
func (store *RedisStore) SyncFixedWindow(ctx context.Context, key string, delta int64, window time.Duration) (int64, error) {
	start := time.Now()
	cmd := store.fixedWindowSyncScript.Run(ctx, store.client, []string{key}, delta, window.Milliseconds())
	store.recorder.ObserveRedisLatency("fixed_window_sync", time.Since(start))

	res, err := cmd.Int64Slice()
	if err != nil {
		return 0, fmt.Errorf("failed to execute fixed window sync: %w", err)
	}
	store.recorder.ObserveLuaExecution("fixed_window_sync", time.Duration(res[1])*time.Microsecond)
	return res[0], nil
}

// SyncTokenBucket refills the bucket for key based on elapsed time, then subtracts
// delta (tokens already consumed locally since the last sync), and returns the
// resulting token count. The result can be negative, signaling local over-admission
// during the interval since the last sync.
func (store *RedisStore) SyncTokenBucket(ctx context.Context, key string, delta float64, capacity float64, rate float64) (float64, error) {
	start := time.Now()
	cmd := store.tokenBucketSyncScript.Run(ctx, store.client, []string{key}, capacity, rate, delta)
	store.recorder.ObserveRedisLatency("token_bucket_sync", time.Since(start))

	res, err := cmd.Slice()
	if err != nil {
		return 0, fmt.Errorf("failed to execute token bucket sync: %w", err)
	}
	tokenStr, ok := res[0].(string)
	if !ok {
		return 0, fmt.Errorf("failed to execute token bucket sync: unexpected token count type %T", res[0])
	}
	remaining, err := strconv.ParseFloat(tokenStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to execute token bucket sync: parse token count: %w", err)
	}
	luaUs, ok := res[1].(int64)
	if !ok {
		return 0, fmt.Errorf("failed to execute token bucket sync: unexpected lua_us type %T", res[1])
	}
	store.recorder.ObserveLuaExecution("token_bucket_sync", time.Duration(luaUs)*time.Microsecond)
	return remaining, nil
}
