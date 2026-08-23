package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBatchingTokenBucketAllowsUpToCapacity verifies local admits are capped
// at capacity using only the initial full-bucket baseline, with no backing store.
func TestBatchingTokenBucketAllowsUpToCapacity(t *testing.T) {
	capacity := float64(3)
	rate := float64(1)
	tb := NewBatchingTokenBucket(capacity, rate)
	ctx := context.Background()

	for call := 1; call <= int(capacity); call++ {
		allowed, err := tb.Allow(ctx, "key")
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", call, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed", call)
		}
	}

	allowed, err := tb.Allow(ctx, "key")
	if err != nil {
		t.Fatalf("unexpected error on drained-bucket call: %v", err)
	}
	if allowed {
		t.Fatal("expected request to be denied after bucket was drained locally")
	}
}

// TestBatchingTokenBucketPendingDelta verifies PendingDelta reports exactly
// the number of tokens consumed locally and not yet reconciled.
func TestBatchingTokenBucketPendingDelta(t *testing.T) {
	tb := NewBatchingTokenBucket(10, 1)
	ctx := context.Background()

	for range 4 {
		if _, err := tb.Allow(ctx, "key"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if delta := tb.PendingDelta(); delta != 4 {
		t.Fatalf("PendingDelta() = %v, want 4", delta)
	}
}

// TestBatchingTokenBucketApplyRemoteTotalCanGoNegative verifies that a
// negative remaining-token count from the backing store (signaling fleet-wide
// over-admission) correctly denies further local requests until a later
// flush restores headroom.
func TestBatchingTokenBucketApplyRemoteTotalCanGoNegative(t *testing.T) {
	tb := NewBatchingTokenBucket(5, 1)
	ctx := context.Background()

	flushed := tb.PendingDelta() // 0 so far; simulate a flush reporting overshoot
	tb.ApplyRemoteTotal(flushed, -2)

	allowed, err := tb.Allow(ctx, "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("expected request to be denied while the reconciled bucket is negative")
	}

	// A later flush restores headroom.
	tb.ApplyRemoteTotal(tb.PendingDelta(), 1)

	allowed, err = tb.Allow(ctx, "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("expected request to be allowed after headroom was restored")
	}
}

// TestBatchingTokenBucketApplyRemoteTotal verifies that reconciling a flush
// only subtracts the flushed amount, so admits that arrived while the flush
// was in flight remain correctly reflected against the new baseline.
func TestBatchingTokenBucketApplyRemoteTotal(t *testing.T) {
	tb := NewBatchingTokenBucket(10, 1)
	ctx := context.Background()

	for range 3 {
		if _, err := tb.Allow(ctx, "key"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	flushed := tb.PendingDelta()
	if flushed != 3 {
		t.Fatalf("PendingDelta() before flush = %v, want 3", flushed)
	}

	for range 2 {
		if _, err := tb.Allow(ctx, "key"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// The backing store reports 7 tokens remaining (10 - 3 flushed).
	tb.ApplyRemoteTotal(flushed, 7)

	if delta := tb.PendingDelta(); delta != 2 {
		t.Fatalf("PendingDelta() after reconciling = %v, want 2 (the in-flight admits)", delta)
	}

	// Baseline is now 7 (remote) - 2 (unreconciled) = 5, so 5 more should be
	// allowed before the bucket is exhausted.
	allowedCount := 0
	for range 10 {
		allowed, err := tb.Allow(ctx, "key")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if allowed {
			allowedCount++
		}
	}
	if allowedCount != 5 {
		t.Fatalf("allowedCount = %d, want 5", allowedCount)
	}
}

// TestBatchingTokenBucketConcurrency verifies BatchingTokenBucket enforces its
// capacity correctly under concurrent load.
func TestBatchingTokenBucketConcurrency(t *testing.T) {
	capacity := float64(100)
	rate := float64(1)
	tb := NewBatchingTokenBucket(capacity, rate)
	ctx := context.Background()

	var wg sync.WaitGroup
	var allowedCount atomic.Int64
	workers := 500

	for range workers {
		wg.Go(func() {
			allowed, err := tb.Allow(ctx, "key")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if allowed {
				allowedCount.Add(1)
			}
		})
	}
	wg.Wait()

	if allowedCount.Load() != int64(capacity) {
		t.Fatalf("allowedCount = %d, want exactly %d", allowedCount.Load(), int64(capacity))
	}
}

// TestBatchingTokenBucketExhausted verifies Exhausted tracks whether Allow can
// currently admit, including after a flush leaves the baseline negative — the
// state a caller's flush cycle must keep reconciling to avoid wedging the
// bucket permanently.
func TestBatchingTokenBucketExhausted(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// setup drives the bucket into the state under test.
		setup func(tb *BatchingTokenBucket)
		want  bool
	}{
		{
			name:  "FreshBucketIsNotExhausted",
			setup: func(*BatchingTokenBucket) {},
			want:  false,
		},
		{
			name: "PartiallyConsumedIsNotExhausted",
			setup: func(tb *BatchingTokenBucket) {
				_, _ = tb.Allow(ctx, "key")
			},
			want: false,
		},
		{
			name: "FullyConsumedIsExhausted",
			setup: func(tb *BatchingTokenBucket) {
				for range 2 {
					_, _ = tb.Allow(ctx, "key")
				}
			},
			want: true,
		},
		{
			name: "NegativeBaselineAfterFlushIsExhausted",
			setup: func(tb *BatchingTokenBucket) {
				for range 2 {
					_, _ = tb.Allow(ctx, "key")
				}
				tb.ApplyRemoteTotal(2, -5)
			},
			want: true,
		},
		{
			name: "RefilledBaselineAfterFlushIsNotExhausted",
			setup: func(tb *BatchingTokenBucket) {
				for range 2 {
					_, _ = tb.Allow(ctx, "key")
				}
				tb.ApplyRemoteTotal(2, -5)
				tb.ApplyRemoteTotal(0, 2)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := NewBatchingTokenBucket(2, 1)
			tt.setup(tb)
			if got := tb.Exhausted(); got != tt.want {
				t.Fatalf("Exhausted() = %v, want %v", got, tt.want)
			}
		})
	}
}
