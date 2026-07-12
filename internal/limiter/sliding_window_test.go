package limiter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestRingBuffer verifies ring-buffer count tracking, eviction, and wrap-around.
func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer(3)
	var oldestTest time.Time

	for i := range 3 {
		ts := time.Now()
		if i == 1 {
			oldestTest = ts
		}
		rb.Push(ts)
	}

	if count := rb.Count(); count != 3 {
		t.Fatalf("expected count 3, got %d", count)
	}

	rb.EvictOldest()
	if count := rb.Count(); count != 2 {
		t.Fatalf("expected count 2, got %d", count)
	}

	rb.Push(time.Now())
	if count := rb.Count(); count != 3 {
		t.Fatalf("expected count 3 after wrap-around, got %d", count)
	}

	if oldest := rb.Oldest(); oldest != oldestTest {
		t.Fatalf("expected the oldest timestamp of %v after wrap-around, but got %v", oldestTest, oldest)
	}

}

// TestSlidingWindow verifies limit enforcement and expiry of old requests.
func TestSlidingWindow(t *testing.T) {
	windowDuration := 100 * time.Millisecond
	limit := int64(2)

	ctx := context.Background()

	tests := []struct {
		name       string
		setupWait  time.Duration
		expectPass bool
	}{
		{name: "Request 1 - Allowed", setupWait: 0, expectPass: true},
		{name: "Request 2 - Allowed (Limit Reached)", setupWait: 0, expectPass: true},
		{name: "Request 2 - Denied (Exceeds Limit)", setupWait: 0, expectPass: false},
		{name: "Request 2 - Allowed (Limit Reached)", setupWait: 150 * time.Millisecond, expectPass: true},
	}

	sw := NewSlidingWindow(limit, windowDuration)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupWait > 0 {
				time.Sleep(tt.setupWait)
			}

			allowed, err := sw.Allow(ctx, "test_key")

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allowed != tt.expectPass {
				t.Errorf("Allow() = %v, want %v", allowed, tt.expectPass)
			}
		})
	}
}

// TestSlidingWindowConcurrency verifies that concurrent requests are capped at the limit.
func TestSlidingWindowConcurrency(t *testing.T) {
	limit := int64(50)
	windowDuration := 1 * time.Second
	sw := NewSlidingWindow(limit, windowDuration)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 100
	allowedCount := 0
	var mu sync.Mutex

	for range workers {
		wg.Go(func() {
			allowed, _ := sw.Allow(ctx, "concurrent_key")
			if allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if int64(allowedCount) != limit {
		t.Errorf("expected exactly %d requests to be allowed, but got %d", limit, allowedCount)
	}
}
