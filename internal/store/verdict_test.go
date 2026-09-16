package store

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestVerdictQuota pins the quota fields every script returns beside the
// verdict — the numbers the RateLimit response headers are built from.
// Remaining counts down by one per admit and clamps at zero; Reset is a
// positive duration no longer than the algorithm's own horizon while the key
// holds state.
func TestVerdictQuota(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	tests := []struct {
		name string
		// allow runs one admission attempt against a fresh key chosen by the
		// test, returning the verdict.
		allow func(t *testing.T, key string) Verdict
		// quota is how many admits a fresh key grants before denying.
		quota int64
		// horizon bounds Reset from above.
		horizon time.Duration
	}{
		{
			name: "fixed window",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				v, err := store.AllowFixedWindow(ctx, key, 3, 5*time.Second)
				if err != nil {
					t.Fatalf("AllowFixedWindow: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 5 * time.Second,
		},
		{
			name: "sliding window",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				v, err := store.AllowSlidingWindow(ctx, key, 3, 5*time.Second)
				if err != nil {
					t.Fatalf("AllowSlidingWindow: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 5 * time.Second,
		},
		{
			name: "token bucket",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				// Refill is slow enough (1 token/s) that the three admits below
				// do not see a token come back mid-test.
				v, err := store.AllowTokenBucket(ctx, key, 3, 1)
				if err != nil {
					t.Fatalf("AllowTokenBucket: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 3 * time.Second, // capacity / rate: time from empty to full
		},
		{
			name: "leaky bucket",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				// 10 per 5s is a 500ms emission interval; burst 3 tolerates two
				// early arrivals, so a fresh key admits three in a row.
				v, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 3)
				if err != nil {
					t.Fatalf("AllowLeakyBucket: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 3 * 500 * time.Millisecond, // burst * emission interval: full schedule catch-up
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := redisTestKey(t, "quota")

			for i := int64(1); i <= tt.quota; i++ {
				v := tt.allow(t, key)
				if !v.Allowed {
					t.Fatalf("admit %d: allowed = false, want true (quota %d)", i, tt.quota)
				}
				if want := tt.quota - i; v.Remaining != want {
					t.Fatalf("admit %d: Remaining = %d, want %d", i, v.Remaining, want)
				}
				if v.Reset <= 0 || v.Reset > tt.horizon {
					t.Fatalf("admit %d: Reset = %s, want in (0, %s]", i, v.Reset, tt.horizon)
				}
			}

			v := tt.allow(t, key)
			if v.Allowed {
				t.Fatalf("admit %d: allowed = true, want denied past quota %d", tt.quota+1, tt.quota)
			}
			if v.Remaining != 0 {
				t.Fatalf("denied: Remaining = %d, want 0", v.Remaining)
			}
			if v.Reset <= 0 || v.Reset > tt.horizon {
				t.Fatalf("denied: Reset = %s, want in (0, %s]", v.Reset, tt.horizon)
			}
		})
	}

	t.Run("leaky bucket retry-after and reset agree on the schedule", func(t *testing.T) {
		key := redisTestKey(t, "gcra")
		first, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 1)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		if !first.Allowed || first.Remaining != 0 || first.RetryAfter != 0 {
			t.Fatalf("first = %+v, want allowed with no remaining burst and no retry-after", first)
		}
		second, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 1)
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if second.Allowed {
			t.Fatal("second: allowed, want denied with no burst tolerance")
		}
		// With burst 1 there is no tolerance, so the next admit and the full
		// catch-up are the same moment: one emission interval away, minus
		// however long the two calls took.
		if second.RetryAfter <= 0 || second.RetryAfter > 500*time.Millisecond {
			t.Fatalf("second: RetryAfter = %s, want in (0, 500ms]", second.RetryAfter)
		}
		if second.Reset != second.RetryAfter {
			t.Fatalf("second: Reset = %s, RetryAfter = %s; with burst 1 they must agree", second.Reset, second.RetryAfter)
		}
	})
}

// TestTracingNestsLuaInsideRedisSpan pins the trace shape a store call
// produces when a tracer is supplied: one client span for the round-trip,
// with the script's own execution time nested inside it as a child whose
// duration is exactly what the script reported. A trace that showed only the
// round-trip would throw away the distinction the metrics already make.
func TestTracingNestsLuaInsideRedisSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	fwScript, swScript, tbScript, lbScript := scripts["fw"], scripts["sw"], scripts["tb"], scripts["lb"]
	fwSyncScript, tbSyncScript := scripts["fw_sync"], scripts["tb_sync"]
	traced := NewRedisStore(globalRedisClient, fwScript, swScript, tbScript, lbScript, fwSyncScript, tbSyncScript, fakeLatencyRecorder{}, provider.Tracer("test"))

	if _, err := traced.AllowFixedWindow(context.Background(), redisTestKey(t, "traced"), 5, time.Second); err != nil {
		t.Fatalf("AllowFixedWindow: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (redis round-trip and nested lua)", len(spans))
	}
	// Spans export in end order: the Lua child is ended first.
	lua, redisSpan := spans[0], spans[1]
	if redisSpan.Name != "redis.script fixed_window" || redisSpan.SpanKind != trace.SpanKindClient {
		t.Fatalf("round-trip span = %q (%s), want \"redis.script fixed_window\" of kind client", redisSpan.Name, redisSpan.SpanKind)
	}
	if lua.Name != "lua fixed_window" {
		t.Fatalf("nested span = %q, want \"lua fixed_window\"", lua.Name)
	}
	if lua.Parent.SpanID() != redisSpan.SpanContext.SpanID() {
		t.Fatal("lua span is not a child of the redis span")
	}
	if lua.StartTime.Before(redisSpan.StartTime) || lua.EndTime.After(redisSpan.EndTime) {
		t.Fatalf("lua span [%s, %s] is not inside the redis span [%s, %s]", lua.StartTime, lua.EndTime, redisSpan.StartTime, redisSpan.EndTime)
	}
	if lua.EndTime.Sub(lua.StartTime) > redisSpan.EndTime.Sub(redisSpan.StartTime) {
		t.Fatal("lua span is longer than the round-trip that contains it")
	}
}

// TestNilTracerProducesNoSpans is the "off" guarantee: a store built with a
// nil tracer must not touch tracing at all, so the request path matches what
// the published benchmarks measured.
func TestNilTracerProducesNoSpans(t *testing.T) {
	store := newTestRedisStore()
	if store.tracer != nil {
		t.Fatal("test store should have no tracer")
	}
	if _, err := store.AllowFixedWindow(context.Background(), redisTestKey(t, "untraced"), 5, time.Second); err != nil {
		t.Fatalf("AllowFixedWindow: %v", err)
	}
}

// recordingLatencyRecorder keeps every Lua execution observation so a test
// can assert the scripts report a real, non-zero self-timing.
type recordingLatencyRecorder struct {
	lua map[string][]time.Duration
}

func (r *recordingLatencyRecorder) ObserveRedisLatency(string, time.Duration) {}
func (r *recordingLatencyRecorder) ObserveLuaExecution(algorithm string, d time.Duration) {
	r.lua[algorithm] = append(r.lua[algorithm], d)
}

// TestLuaExecutionTimeIsMeasured guards a regression that went unnoticed
// for months: redis.call('TIME') is frozen for the whole of a script's
// execution, so timing a script with it always reports zero, and the
// limigo_lua_execution_seconds histogram silently recorded nothing but
// zeros. The scripts now time themselves with os.clock() (Redis 7.4+),
// which advances; this test fails if any script goes back to a clock that
// does not.
func TestLuaExecutionTimeIsMeasured(t *testing.T) {
	recorder := &recordingLatencyRecorder{lua: map[string][]time.Duration{}}
	fwScript, swScript, tbScript, lbScript := scripts["fw"], scripts["sw"], scripts["tb"], scripts["lb"]
	fwSyncScript, tbSyncScript := scripts["fw_sync"], scripts["tb_sync"]
	store := NewRedisStore(globalRedisClient, fwScript, swScript, tbScript, lbScript, fwSyncScript, tbSyncScript, recorder, nil)
	ctx := context.Background()

	calls := map[string]func() error{
		"fixed_window": func() error {
			_, err := store.AllowFixedWindow(ctx, redisTestKey(t, "fw"), 10, time.Second)
			return err
		},
		"sliding_window": func() error {
			_, err := store.AllowSlidingWindow(ctx, redisTestKey(t, "sw"), 10, time.Second)
			return err
		},
		"token_bucket": func() error {
			_, err := store.AllowTokenBucket(ctx, redisTestKey(t, "tb"), 10, 1)
			return err
		},
		"leaky_bucket": func() error {
			_, err := store.AllowLeakyBucket(ctx, redisTestKey(t, "lb"), 10, time.Second, 1)
			return err
		},
		"fixed_window_sync": func() error {
			_, err := store.SyncFixedWindow(ctx, redisTestKey(t, "fws"), 1, time.Second)
			return err
		},
		"token_bucket_sync": func() error {
			_, err := store.SyncTokenBucket(ctx, redisTestKey(t, "tbs"), 1, 10, 1)
			return err
		},
	}
	for algorithm, call := range calls {
		t.Run(algorithm, func(t *testing.T) {
			// A handful of calls: os.clock() has microsecond resolution and a
			// script can finish inside one tick, so the claim is "measured at
			// least once", not "never zero".
			for range 20 {
				if err := call(); err != nil {
					t.Fatalf("%s: %v", algorithm, err)
				}
			}
			var max time.Duration
			for _, d := range recorder.lua[algorithm] {
				if d > max {
					max = d
				}
			}
			if max <= 0 {
				t.Fatalf("%s reported a Lua execution time of zero on every call; the script's clock is not advancing", algorithm)
			}
		})
	}
}

// TestPeekDoesNotConsume pins the inspection contract for every algorithm:
// a peek reports what the next request would see — the same Allowed and
// Remaining that Allow then actually returns before it consumes anything —
// and peeking any number of times changes nothing.
func TestPeekDoesNotConsume(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	type algo struct {
		name  string
		peek  func(key string) (Verdict, error)
		allow func(key string) (Verdict, error)
		quota int64
	}
	algos := []algo{
		{
			name:  "fixed window",
			peek:  func(k string) (Verdict, error) { return store.PeekFixedWindow(ctx, k, 3, 5*time.Second) },
			allow: func(k string) (Verdict, error) { return store.AllowFixedWindow(ctx, k, 3, 5*time.Second) },
			quota: 3,
		},
		{
			name:  "sliding window",
			peek:  func(k string) (Verdict, error) { return store.PeekSlidingWindow(ctx, k, 3, 5*time.Second) },
			allow: func(k string) (Verdict, error) { return store.AllowSlidingWindow(ctx, k, 3, 5*time.Second) },
			quota: 3,
		},
		{
			name:  "token bucket",
			peek:  func(k string) (Verdict, error) { return store.PeekTokenBucket(ctx, k, 3, 1) },
			allow: func(k string) (Verdict, error) { return store.AllowTokenBucket(ctx, k, 3, 1) },
			quota: 3,
		},
		{
			name:  "leaky bucket",
			peek:  func(k string) (Verdict, error) { return store.PeekLeakyBucket(ctx, k, 10, 5*time.Second, 3) },
			allow: func(k string) (Verdict, error) { return store.AllowLeakyBucket(ctx, k, 10, 5*time.Second, 3) },
			quota: 3,
		},
	}
	for _, a := range algos {
		t.Run(a.name, func(t *testing.T) {
			key := redisTestKey(t, "peek")

			// A fresh key: peek says the full quota is available, repeatedly.
			for i := range 3 {
				v, err := a.peek(key)
				if err != nil {
					t.Fatalf("peek %d on fresh key: %v", i+1, err)
				}
				if !v.Allowed || v.Remaining != a.quota {
					t.Fatalf("peek %d on fresh key = %+v, want allowed with %d remaining", i+1, v, a.quota)
				}
			}

			// Interleave: what peek predicts, allow then delivers.
			for i := int64(1); i <= a.quota; i++ {
				predicted, err := a.peek(key)
				if err != nil {
					t.Fatalf("peek before admit %d: %v", i, err)
				}
				actual, err := a.allow(key)
				if err != nil {
					t.Fatalf("admit %d: %v", i, err)
				}
				if predicted.Allowed != actual.Allowed {
					t.Fatalf("admit %d: peek predicted allowed=%v, allow returned %v", i, predicted.Allowed, actual.Allowed)
				}
				// Peek's remaining includes the request about to be made; allow's
				// remaining is after it.
				if predicted.Remaining != actual.Remaining+1 {
					t.Fatalf("admit %d: peek remaining %d, allow remaining %d; want peek = allow + 1", i, predicted.Remaining, actual.Remaining)
				}
			}

			// Exhausted: peek says so, and still consumes nothing.
			v, err := a.peek(key)
			if err != nil {
				t.Fatalf("peek when exhausted: %v", err)
			}
			if v.Allowed || v.Remaining != 0 {
				t.Fatalf("peek when exhausted = %+v, want denied with 0 remaining", v)
			}
			if v.Reset <= 0 {
				t.Fatalf("peek when exhausted: Reset = %s, want > 0 while the key holds state", v.Reset)
			}
		})
	}

	t.Run("leaky bucket peek reports retry-after when denied", func(t *testing.T) {
		key := redisTestKey(t, "peek-gcra")
		if _, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 1); err != nil {
			t.Fatalf("allow: %v", err)
		}
		v, err := store.PeekLeakyBucket(ctx, key, 10, 5*time.Second, 1)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if v.Allowed || v.RetryAfter <= 0 || v.RetryAfter > 500*time.Millisecond {
			t.Fatalf("peek = %+v, want denied with retry-after in (0, 500ms]", v)
		}
	})
}
