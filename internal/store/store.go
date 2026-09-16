package store

import (
	"context"
	"time"
)

// Verdict is the outcome of one admission decision, as computed server-side
// by the algorithm's Lua script. Beyond the allow/deny bit it carries the
// quota state the script already had in hand, so the caller can report it
// (RateLimit response headers, quota inspection) without a second round-trip.
type Verdict struct {
	// Allowed reports whether the request is within limit.
	Allowed bool
	// Remaining is how many further requests the key could have admitted
	// immediately after this decision, in requests. Never negative.
	Remaining int64
	// Reset is how long until the key's quota is fully restored: the fixed
	// window rolling over, the oldest sliding-window entry falling out, the
	// token bucket refilling to capacity, or the GCRA schedule catching up.
	// Zero when the key holds no state (nothing to restore).
	Reset time.Duration
	// RetryAfter is the exact duration until the key will next admit a
	// request, when the algorithm can compute it (currently only leaky
	// bucket) and the request was denied. Zero otherwise.
	RetryAfter time.Duration
}

// FixedWindowStore is the backing store interface for the fixed window algorithm.
// Implementations must execute the read-increment-check atomically to prevent
// race conditions in a multi-node deployment.
type FixedWindowStore interface {
	// AllowFixedWindow increments the request count for key within the current
	// window and reports whether the count is within limit. Verdict.Reset is
	// the time left in the current window.
	AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (Verdict, error)
}

// SlidingWindowStore is the backing store interface for the sliding window algorithm.
// Implementations must execute the read-increment-check atomically to prevent
// race conditions in a multi-node deployment.
type SlidingWindowStore interface {
	// AllowSlidingWindow records the current request for key and reports whether
	// the number of requests within the rolling window is within limit.
	// Verdict.Reset is the time until the oldest recorded request leaves the
	// window.
	AllowSlidingWindow(ctx context.Context, key string, limit int64, window time.Duration) (Verdict, error)
}

// TokenBucketStore is the backing store interface for the token bucket algorithm.
// Implementations must execute the refill-and-consume atomically to prevent
// race conditions in a multi-node deployment.
type TokenBucketStore interface {
	// AllowTokenBucket attempts to consume one token from the bucket for key,
	// refilling based on elapsed time and rate. Verdict.Remaining is the whole
	// tokens left after the attempt; Verdict.Reset is the time until the bucket
	// is back at capacity.
	AllowTokenBucket(ctx context.Context, key string, capacity float64, rate float64) (Verdict, error)
}

// LeakyBucketStore is the backing store interface for the leaky bucket
// algorithm (GCRA). Implementations must execute the schedule-check-and-advance
// atomically to prevent race conditions in a multi-node deployment.
type LeakyBucketStore interface {
	// AllowLeakyBucket admits one request against key's GCRA schedule, derived
	// from limit, window, and burst, reporting whether the request arrived on
	// schedule. When denied, Verdict.RetryAfter is the exact duration until the
	// schedule will next admit a request for key. Verdict.Remaining is how many
	// more requests the burst tolerance would admit right now; Verdict.Reset
	// is the time until the schedule has fully caught up.
	AllowLeakyBucket(ctx context.Context, key string, limit int64, window time.Duration, burst int64) (Verdict, error)
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
