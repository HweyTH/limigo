package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBatchingFixedWindowAllowsUpToLimit verifies local admits are capped at
// limit using only the zero-value remote baseline, with no backing store.
func TestBatchingFixedWindowAllowsUpToLimit(t *testing.T) {
	limit := int64(3)
	bw := NewBatchingFixedWindow(limit)
	ctx := context.Background()

	for i := range limit {
		allowed, err := bw.Allow(ctx, "key")
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed", i+1)
		}
	}

	allowed, err := bw.Allow(ctx, "key")
	if err != nil {
		t.Fatalf("unexpected error on over-limit call: %v", err)
	}
	if allowed {
		t.Fatal("expected request to be denied after local limit exceeded")
	}
}

// TestBatchingFixedWindowPendingDelta verifies PendingDelta reports exactly
// the number of local admits not yet reconciled with a backing store.
func TestBatchingFixedWindowPendingDelta(t *testing.T) {
	bw := NewBatchingFixedWindow(10)
	ctx := context.Background()

	for range 4 {
		if _, err := bw.Allow(ctx, "key"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if delta := bw.PendingDelta(); delta != 4 {
		t.Fatalf("PendingDelta() = %d, want 4", delta)
	}
}

// TestBatchingFixedWindowApplyRemoteTotal verifies that reconciling a flush
// only subtracts the flushed amount, so admits that arrived while the flush
// was in flight remain correctly reflected against the new baseline.
func TestBatchingFixedWindowApplyRemoteTotal(t *testing.T) {
	bw := NewBatchingFixedWindow(10)
	ctx := context.Background()

	for range 3 {
		if _, err := bw.Allow(ctx, "key"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	flushed := bw.PendingDelta()
	if flushed != 3 {
		t.Fatalf("PendingDelta() before flush = %d, want 3", flushed)
	}

	// Simulate two more local admits arriving while the (simulated) flush is
	// in flight, i.e. after the delta to flush was read but before the
	// backing store's response is reconciled.
	for range 2 {
		if _, err := bw.Allow(ctx, "key"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// The backing store reports an authoritative total of 3 (matching what
	// was flushed, as if no other node admitted anything in the meantime).
	bw.ApplyRemoteTotal(flushed, 3)

	if delta := bw.PendingDelta(); delta != 2 {
		t.Fatalf("PendingDelta() after reconciling = %d, want 2 (the in-flight admits)", delta)
	}

	// Baseline is now 3 (remote) + 2 (unreconciled) = 5, so 5 more should be
	// allowed before hitting the limit of 10.
	allowedCount := 0
	for range 10 {
		allowed, err := bw.Allow(ctx, "key")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if allowed {
			allowedCount++
		}
	}
	if allowedCount != 5 {
		t.Fatalf("allowedCount = %d, want 5 (limit 10 - baseline 5)", allowedCount)
	}
}

// TestBatchingFixedWindowConcurrency verifies BatchingFixedWindow enforces its
// limit correctly under concurrent load.
func TestBatchingFixedWindowConcurrency(t *testing.T) {
	limit := int64(100)
	bw := NewBatchingFixedWindow(limit)
	ctx := context.Background()

	var wg sync.WaitGroup
	var allowedCount atomic.Int64
	workers := 500

	for range workers {
		wg.Go(func() {
			allowed, err := bw.Allow(ctx, "key")
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

	if allowedCount.Load() != limit {
		t.Fatalf("allowedCount = %d, want exactly %d", allowedCount.Load(), limit)
	}
}

// TestBatchingFixedWindowExhausted verifies Exhausted tracks whether Allow can
// currently admit, including after a flush reconciles the baseline to the
// limit — the state a caller's flush cycle must keep reconciling to avoid
// wedging the window permanently.
func TestBatchingFixedWindowExhausted(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// setup drives the window into the state under test.
		setup func(bw *BatchingFixedWindow)
		want  bool
	}{
		{
			name:  "FreshWindowIsNotExhausted",
			setup: func(*BatchingFixedWindow) {},
			want:  false,
		},
		{
			name: "PartiallyConsumedIsNotExhausted",
			setup: func(bw *BatchingFixedWindow) {
				_, _ = bw.Allow(ctx, "key")
			},
			want: false,
		},
		{
			name: "AtLimitIsExhausted",
			setup: func(bw *BatchingFixedWindow) {
				for range 2 {
					_, _ = bw.Allow(ctx, "key")
				}
			},
			want: true,
		},
		{
			name: "BaselineAtLimitAfterFlushIsExhausted",
			setup: func(bw *BatchingFixedWindow) {
				_, _ = bw.Allow(ctx, "key")
				bw.ApplyRemoteTotal(1, 2)
			},
			want: true,
		},
		{
			name: "RolledWindowAfterFlushIsNotExhausted",
			setup: func(bw *BatchingFixedWindow) {
				_, _ = bw.Allow(ctx, "key")
				bw.ApplyRemoteTotal(1, 2)
				bw.ApplyRemoteTotal(0, 0)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bw := NewBatchingFixedWindow(2)
			tt.setup(bw)
			if got := bw.Exhausted(); got != tt.want {
				t.Fatalf("Exhausted() = %v, want %v", got, tt.want)
			}
		})
	}
}
