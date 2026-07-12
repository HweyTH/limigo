package limiter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestLeakyBucket verifies GCRA scheduling: burst tolerance admits a run of
// back-to-back requests, subsequent requests are throttled until the schedule
// catches up, and admission resumes once the emission interval elapses.
func TestLeakyBucket(t *testing.T) {
	limit := int64(10)
	window := 100 * time.Millisecond
	burst := int64(2)
	// emission interval = window/limit = 10ms; tolerance = interval*(burst-1) = 10ms.

	ctx := context.Background()

	tests := []struct {
		name       string
		setupWait  time.Duration
		expectPass bool
	}{
		{name: "Request 1 - Allowed", setupWait: 0, expectPass: true},
		{name: "Request 2 - Allowed (within burst tolerance)", setupWait: 0, expectPass: true},
		{name: "Request 3 - Denied (arrives too early)", setupWait: 0, expectPass: false},
		{name: "Request 4 - Allowed (waited for schedule)", setupWait: 15 * time.Millisecond, expectPass: true},
	}

	lb := NewLeakyBucket(limit, window, burst)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupWait > 0 {
				time.Sleep(tt.setupWait)
			}

			allowed, err := lb.Allow(ctx, "test_key")

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allowed != tt.expectPass {
				t.Errorf("Allow() = %v, want %v", allowed, tt.expectPass)
			}
		})
	}
}

// TestLeakyBucketNoBurstTolerance verifies that a burst of 1 (or 0, normalised
// to 1) admits strictly on schedule with no clumping tolerance at all.
func TestLeakyBucketNoBurstTolerance(t *testing.T) {
	limit := int64(10)
	window := 100 * time.Millisecond // emission interval = 10ms, tolerance = 0.
	ctx := context.Background()

	lb := NewLeakyBucket(limit, window, 1)

	allowed, err := lb.Allow(ctx, "test_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected first request to be allowed")
	}

	allowed, err = lb.Allow(ctx, "test_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatalf("expected immediate second request to be denied with no burst tolerance")
	}

	time.Sleep(15 * time.Millisecond)
	allowed, err = lb.Allow(ctx, "test_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected request to be allowed after the emission interval elapsed")
	}
}

// TestLeakyBucketConcurrency verifies that concurrent requests are capped at
// the schedule's steady-state admission rate over a fixed observation window.
func TestLeakyBucketConcurrency(t *testing.T) {
	limit := int64(50)
	window := 1 * time.Second // emission interval = 20ms.
	lb := NewLeakyBucket(limit, window, 1)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 100
	allowedCount := 0
	var mu sync.Mutex

	for range workers {
		wg.Go(func() {
			allowed, _ := lb.Allow(ctx, "concurrent_key")
			if allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	// With no burst tolerance, only the request that establishes the initial
	// TAT is guaranteed to be admitted in a tight concurrent burst — the rest
	// arrive before the schedule allows the next one.
	if allowedCount < 1 {
		t.Errorf("expected at least 1 request to be allowed, got %d", allowedCount)
	}
	if int64(allowedCount) > limit {
		t.Errorf("expected at most %d requests to be allowed, got %d", limit, allowedCount)
	}
}
