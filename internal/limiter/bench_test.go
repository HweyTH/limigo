package limiter

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// Pure-algorithm benchmarks (ADR-0001): the innermost layer of the
// three-layer benchmark story. These measure algorithm cost in isolation —
// no store, no HTTP, no container — so later, store- and network-backed
// measurements can attribute their cost correctly instead of guessing.
//
// Serial benchmarks call Allow sequentially against a single key on a single
// algorithm instance, in one goroutine: the per-operation cost of the
// algorithm itself, uncontended.
//
// Parallel benchmarks spread concurrent callers across many keys through the
// algorithm's manager, where one exists (BucketManager, WindowManager,
// LeakyBucketManager, or a Batching*Manager). This is what exposes
// contention on the manager's per-key routing lock, which concurrent
// production traffic actually hits and a serial benchmark cannot see.
// FixedWindow has no manager in this codebase (uncached fixed-window rules
// are evaluated directly against Redis — see internal/rules/compile.go), so
// its parallel benchmark pre-allocates one instance per key instead, to
// still exercise many-key concurrent access.
//
// Limits are sized generously relative to each benchmark's iteration count so
// a run settles into (and stays in) admitting steady state instead of
// exhausting its quota partway through and spending the rest of the run
// measuring the cheaper deny path — that regime shift is what would make two
// runs disagree.

const benchNumKeys = 1000

var benchCtx = context.Background()

func BenchmarkFixedWindow_Serial(b *testing.B) {
	runSerialBench(b, NewFixedWindow(1_000_000_000, time.Minute))
}

func BenchmarkFixedWindow_Parallel(b *testing.B) {
	windows := make([]*FixedWindow, benchNumKeys)
	for i := range windows {
		windows[i] = NewFixedWindow(1_000_000_000, time.Minute)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = windows[i%benchNumKeys].Allow(benchCtx, "key")
			i++
		}
	})
}

// SlidingWindow's ring buffer allocates memory proportional to limit, and the
// parallel benchmark's manager creates one instance per key — a huge limit
// (the trick used for the other algorithms below) would multiply out to
// gigabytes across benchNumKeys instances. A short window is used instead:
// entries expire and get evicted within microseconds, so the benchmark
// settles into a steady evict/admit cycle rather than exhausting once and
// staying denied for the rest of the run.
const slidingWindowBenchLimit = 1000

func BenchmarkSlidingWindow_Serial(b *testing.B) {
	runSerialBench(b, NewSlidingWindow(slidingWindowBenchLimit, time.Millisecond))
}

func BenchmarkSlidingWindow_Parallel(b *testing.B) {
	runParallelBench(b, NewWindowManager(slidingWindowBenchLimit, time.Millisecond))
}

func BenchmarkTokenBucket_Serial(b *testing.B) {
	runSerialBench(b, NewTokenBucket(1_000_000_000, 1_000_000_000))
}

func BenchmarkTokenBucket_Parallel(b *testing.B) {
	runParallelBench(b, NewBucketManager(1_000_000_000, 1_000_000_000))
}

func BenchmarkLeakyBucket_Serial(b *testing.B) {
	runSerialBench(b, NewLeakyBucket(1_000_000_000, time.Second, 10))
}

func BenchmarkLeakyBucket_Parallel(b *testing.B) {
	runParallelBench(b, NewLeakyBucketManager(1_000_000_000, time.Second, 10))
}

func BenchmarkBatchingFixedWindow_Serial(b *testing.B) {
	runSerialBench(b, NewBatchingFixedWindow(1_000_000_000))
}

func BenchmarkBatchingFixedWindow_Parallel(b *testing.B) {
	runParallelBench(b, NewBatchingFixedWindowManager(1_000_000_000))
}

func BenchmarkBatchingTokenBucket_Serial(b *testing.B) {
	runSerialBench(b, NewBatchingTokenBucket(1_000_000_000, 1_000_000_000))
}

func BenchmarkBatchingTokenBucket_Parallel(b *testing.B) {
	runParallelBench(b, NewBatchingTokenBucketManager(1_000_000_000, 1_000_000_000))
}

// runSerialBench measures the per-operation cost of a Limiter's Allow, called
// sequentially against a single key by a single goroutine.
func runSerialBench(b *testing.B, l Limiter) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = l.Allow(benchCtx, "key")
	}
}

// runParallelBench measures a Limiter's Allow under concurrent callers
// spread across benchNumKeys distinct keys, exposing any contention on the
// Limiter's own key-routing lock.
func runParallelBench(b *testing.B, l Limiter) {
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := strconv.Itoa(i % benchNumKeys)
			_, _ = l.Allow(benchCtx, key)
			i++
		}
	})
}
