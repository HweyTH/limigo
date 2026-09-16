package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
	// tracer is nil when tracing is off, which keeps the hot path free of
	// even a no-op span: the published benchmarks were measured without
	// tracing, and "off" must mean the same code path they ran.
	tracer trace.Tracer
}

// NewRedisStore returns a RedisStore that runs its scripts on redisClient, which
// may be any go-redis client type that implements redis.Scripter (standalone,
// cluster, or ring).
// fwScript, swScript, tbScript, and lbScript are the Lua source strings for the fixed
// window, sliding window, token bucket, and leaky bucket algorithms respectively.
// fwSyncScript and tbSyncScript are the Lua source strings for the batched delta-sync
// variants of the fixed window and token bucket algorithms, used by node-local caching.
// recorder observes the round-trip latency of every script execution, labeled by algorithm.
// tracer, when non-nil, gets a client span per script execution with the script's
// own execution time nested inside it; nil disables tracing at no cost.
func NewRedisStore(redisClient redis.Scripter, fwScript string, swScript string, tbScript string, lbScript string, fwSyncScript string, tbSyncScript string, recorder LatencyRecorder, tracer trace.Tracer) *RedisStore {
	newRedisStore := RedisStore{
		client:                redisClient,
		fixedWindowScript:     redis.NewScript(fwScript),
		slidingWindowScript:   redis.NewScript(swScript),
		tokenBucketScript:     redis.NewScript(tbScript),
		leakyBucketScript:     redis.NewScript(lbScript),
		fixedWindowSyncScript: redis.NewScript(fwSyncScript),
		tokenBucketSyncScript: redis.NewScript(tbSyncScript),
		recorder:              recorder,
		tracer:                tracer,
	}
	return &newRedisStore
}

// exec runs script against key with args, timing the round-trip for the
// recorder and, when tracing is on, opening a client span for it. The
// returned finish must be called once the reply has been parsed: with the
// script's self-reported execution time and a nil error on success, or with
// the error otherwise. It records the Lua time and closes the span.
//
// The Lua span is nested inside the round-trip span with its exact reported
// duration, but its position inside the round-trip is an estimate: the
// script's own clock says how long it ran, not when, so it is centred. What
// the trace shows truthfully is the proportion — how much of the Redis leg
// was the script and how much was the network and the client.
func (store *RedisStore) exec(ctx context.Context, algorithm string, script *redis.Script, key string, args ...any) (*redis.Cmd, func(lua time.Duration, err error)) {
	var span trace.Span
	if store.tracer != nil {
		ctx, span = store.tracer.Start(ctx, "redis.script "+algorithm,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system.name", "redis"),
				attribute.String("limigo.algorithm", algorithm),
			),
		)
	}
	start := time.Now()
	cmd := script.Run(ctx, store.client, []string{key}, args...)
	roundTrip := time.Since(start)
	store.recorder.ObserveRedisLatency(algorithm, roundTrip)

	finish := func(lua time.Duration, err error) {
		if err == nil {
			store.recorder.ObserveLuaExecution(algorithm, lua)
		}
		if span == nil {
			return
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			// Emitted even at zero length: a script that ran inside one
			// microsecond of Redis's clock is a real reading, not a missing one.
			luaStart := start
			if lua < roundTrip {
				luaStart = start.Add((roundTrip - lua) / 2)
			}
			_, luaSpan := store.tracer.Start(ctx, "lua "+algorithm,
				trace.WithTimestamp(luaStart),
				trace.WithAttributes(attribute.String("limigo.algorithm", algorithm)),
			)
			luaSpan.End(trace.WithTimestamp(luaStart.Add(lua)))
		}
		span.End()
	}
	return cmd, finish
}

// AllowFixedWindow increments the request counter for key within the current window
// and reports whether the count is within limit. The window resets automatically
// when the Redis key expires; Verdict.Reset is the time left until it does.
func (store *RedisStore) AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (Verdict, error) {
	cmd, finish := store.exec(ctx, "fixed_window", store.fixedWindowScript, key, limit, window.Milliseconds())
	res, err := int64Reply(cmd, 4)
	if err != nil {
		finish(0, err)
		return Verdict{}, fmt.Errorf("failed to execute fixed window algorithm: %w", err)
	}
	finish(time.Duration(res[1])*time.Microsecond, nil)
	return Verdict{
		Allowed:   res[0] == 1,
		Remaining: res[2],
		Reset:     time.Duration(res[3]) * time.Millisecond,
	}, nil
}

// AllowSlidingWindow records the current request for key in a sliding window and
// reports whether the number of requests within the rolling window is within
// limit. Verdict.Reset is the time until the oldest recorded request leaves the
// window.
func (store *RedisStore) AllowSlidingWindow(ctx context.Context, key string, limit int64, window time.Duration) (Verdict, error) {
	cmd, finish := store.exec(ctx, "sliding_window", store.slidingWindowScript, key, limit, window.Milliseconds())
	res, err := int64Reply(cmd, 4)
	if err != nil {
		finish(0, err)
		return Verdict{}, fmt.Errorf("failed to execute sliding window algorithm: %w", err)
	}
	finish(time.Duration(res[1])*time.Microsecond, nil)
	return Verdict{
		Allowed:   res[0] == 1,
		Remaining: res[2],
		Reset:     time.Duration(res[3]) * time.Millisecond,
	}, nil
}

// AllowTokenBucket attempts to consume one token from the bucket for key, refilling
// based on elapsed time and rate. Verdict.Remaining is the whole tokens left after
// the attempt; Verdict.Reset is the time until the bucket is back at capacity.
func (store *RedisStore) AllowTokenBucket(ctx context.Context, key string, capacity float64, rate float64) (Verdict, error) {
	cmd, finish := store.exec(ctx, "token_bucket", store.tokenBucketScript, key, capacity, rate)
	res, err := int64Reply(cmd, 4)
	if err != nil {
		finish(0, err)
		return Verdict{}, fmt.Errorf("failed to execute token bucket algorithm: %w", err)
	}
	finish(time.Duration(res[1])*time.Microsecond, nil)
	return Verdict{
		Allowed:   res[0] == 1,
		Remaining: res[2],
		Reset:     time.Duration(res[3]) * time.Millisecond,
	}, nil
}

// AllowLeakyBucket admits one request against key's GCRA schedule, reporting
// whether the request arrived on schedule or too early. limit and window define
// the emission interval (window / limit); burst defines the tolerance for clumped
// arrivals (emission_interval * (burst - 1)). burst values less than 1 are treated
// as 1 (no clumping tolerance). When denied, Verdict.RetryAfter is the exact
// duration until key's schedule will next admit a request. Verdict.Remaining is
// how many further requests the tolerance would admit right now; Verdict.Reset
// is the time until the schedule has fully caught up.
func (store *RedisStore) AllowLeakyBucket(ctx context.Context, key string, limit int64, window time.Duration, burst int64) (Verdict, error) {
	if burst < 1 {
		burst = 1
	}
	emissionIntervalMs := float64(window.Milliseconds()) / float64(limit)
	toleranceMs := emissionIntervalMs * float64(burst-1)

	cmd, finish := store.exec(ctx, "leaky_bucket", store.leakyBucketScript, key, emissionIntervalMs, toleranceMs)
	res, err := int64Reply(cmd, 5)
	if err != nil {
		finish(0, err)
		return Verdict{}, fmt.Errorf("failed to execute leaky bucket algorithm: %w", err)
	}
	finish(time.Duration(res[2])*time.Microsecond, nil)
	return Verdict{
		Allowed:    res[0] == 1,
		RetryAfter: time.Duration(res[1]) * time.Millisecond,
		Remaining:  res[3],
		Reset:      time.Duration(res[4]) * time.Millisecond,
	}, nil
}

// int64Reply reads a script's integer-array reply and checks it has exactly
// want elements, so a script and its Go caller that disagree about the reply
// shape fail with a clear error instead of an index panic on the hot path.
func int64Reply(cmd *redis.Cmd, want int) ([]int64, error) {
	res, err := cmd.Int64Slice()
	if err != nil {
		return nil, err
	}
	if len(res) != want {
		return nil, fmt.Errorf("script returned %d values, want %d", len(res), want)
	}
	return res, nil
}

// SyncFixedWindow folds delta (requests already admitted locally since the last
// sync) into the authoritative counter for key and returns the resulting total.
func (store *RedisStore) SyncFixedWindow(ctx context.Context, key string, delta int64, window time.Duration) (int64, error) {
	cmd, finish := store.exec(ctx, "fixed_window_sync", store.fixedWindowSyncScript, key, delta, window.Milliseconds())
	res, err := int64Reply(cmd, 2)
	if err != nil {
		finish(0, err)
		return 0, fmt.Errorf("failed to execute fixed window sync: %w", err)
	}
	finish(time.Duration(res[1])*time.Microsecond, nil)
	return res[0], nil
}

// SyncTokenBucket refills the bucket for key based on elapsed time, then subtracts
// delta (tokens already consumed locally since the last sync), and returns the
// resulting token count. The result can be negative, signaling local over-admission
// during the interval since the last sync.
func (store *RedisStore) SyncTokenBucket(ctx context.Context, key string, delta float64, capacity float64, rate float64) (float64, error) {
	cmd, finish := store.exec(ctx, "token_bucket_sync", store.tokenBucketSyncScript, key, capacity, rate, delta)
	remaining, lua, err := parseTokenBucketSync(cmd)
	if err != nil {
		finish(0, err)
		return 0, fmt.Errorf("failed to execute token bucket sync: %w", err)
	}
	finish(lua, nil)
	return remaining, nil
}

// parseTokenBucketSync reads token_bucket_sync.lua's {tokens, lua_us} reply.
// The token count travels as a string because Redis truncates Lua numbers to
// integers on return, which would drop a partially refilled bucket's fraction.
func parseTokenBucketSync(cmd *redis.Cmd) (remaining float64, lua time.Duration, err error) {
	res, err := cmd.Slice()
	if err != nil {
		return 0, 0, err
	}
	if len(res) != 2 {
		return 0, 0, fmt.Errorf("script returned %d values, want 2", len(res))
	}
	tokenStr, ok := res[0].(string)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected token count type %T", res[0])
	}
	remaining, err = strconv.ParseFloat(tokenStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse token count: %w", err)
	}
	luaUs, ok := res[1].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected lua_us type %T", res[1])
	}
	return remaining, time.Duration(luaUs) * time.Microsecond, nil
}
