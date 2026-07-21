package store

import (
	"context"
	"time"
)

// FixedWindowStore is the backing store interface for the fixed window algorithm.
// Implementations must execute the read-increment-check atomically to prevent
// race conditions in a multi-node deployment.
type FixedWindowStore interface {
	// AllowFixedWindow increments the request count for key within the current window and
	// returns true if the count is within limit, false if it should be throttled.
	AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error)
}

// SlidingWindowStore is the backing store interface for the sliding window algorithm.
// Implementations must execute the read-increment-check atomically to prevent
// race conditions in a multi-node deployment.
type SlidingWindowStore interface {
	// AllowSlidingWindow records the current request for key and returns true if the number of
	// requests within the rolling window is within limit, false if it should be throttled.
	AllowSlidingWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error)
}

// TokenBucketStore is the backing store interface for the token bucket algorithm.
// Implementations must execute the refill-and-consume atomically to prevent
// race conditions in a multi-node deployment.
type TokenBucketStore interface {
	// AllowTokenBucket attempts to consume one token from the bucket for key, refilling based
	// on elapsed time and rate. Returns true if a token was consumed, false if the
	// bucket is empty and the request should be throttled.
	AllowTokenBucket(ctx context.Context, key string, capacity float64, rate float64) (bool, error)
}

// LeakyBucketStore is the backing store interface for the leaky bucket
// algorithm (GCRA). Implementations must execute the schedule-check-and-advance
// atomically to prevent race conditions in a multi-node deployment.
type LeakyBucketStore interface {
	// AllowLeakyBucket admits one request against key's GCRA schedule, derived
	// from limit, window, and burst, returning true if the request arrived on
	// schedule, false if it arrived too early and should be throttled. When
	// denied, retryAfter is the exact duration until the schedule will next
	// admit a request for key; it is zero when allowed.
	AllowLeakyBucket(ctx context.Context, key string, limit int64, window time.Duration, burst int64) (allowed bool, retryAfter time.Duration, err error)
}

// FixedWindowSyncStore reconciles a batch of already-admitted local requests
// with the authoritative fixed window counter, for callers using node-local
// caching to absorb bursts between synchronous Redis round-trips.
type FixedWindowSyncStore interface {
	// SyncFixedWindow folds delta (requests already admitted locally since the
	// last sync) into the authoritative counter for key and returns the
	// resulting total, so the caller can recalibrate its local baseline.
	SyncFixedWindow(ctx context.Context, key string, delta int64, window time.Duration) (total int64, err error)
}

// TokenBucketSyncStore reconciles a batch of already-consumed local tokens
// with the authoritative token bucket state, for callers using node-local
// caching to absorb bursts between synchronous Redis round-trips.
type TokenBucketSyncStore interface {
	// SyncTokenBucket refills the bucket for key based on elapsed time, then
	// subtracts delta (tokens already consumed locally since the last sync),
	// and returns the resulting token count so the caller can recalibrate its
	// local baseline. The result can be negative, signaling local over-admission
	// during the interval since the last sync.
	SyncTokenBucket(ctx context.Context, key string, delta float64, capacity float64, rate float64) (remaining float64, err error)
}

// LatencyRecorder receives timing observations for Redis round trips and
// server-side Lua execution, keyed by the algorithm that produced them.
type LatencyRecorder interface {
	ObserveRedisLatency(algorithm string, d time.Duration)
	ObserveLuaExecution(algorithm string, d time.Duration)
}
