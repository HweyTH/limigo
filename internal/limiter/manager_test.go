package limiter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBucketManagerIsolation verifies that separate keys maintain independent
// token bucket state — exhausting one key's bucket must not affect another.
func TestBucketManagerIsolation(t *testing.T) {
	capacity := int64(2)
	refillRate := 1.0
	bm := NewBucketManager(float64(capacity), refillRate)
	ctx := context.Background()

	ip1 := "192.168.1.100"
	ip2 := "10.0.0.5"

	bm.Allow(ctx, ip1)
	bm.Allow(ctx, ip1)

	allowed, err := bm.Allow(ctx, ip1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding limit", ip1)
	}

	allowed, err = bm.Allow(ctx, ip2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// TestBucketManagerConcurrency verifies that BucketManager is safe for concurrent
// use — covering both concurrent writes across distinct keys and concurrent reads
// on a single shared key.
func TestBucketManagerConcurrency(t *testing.T) {
	capacity := int64(2)
	refillRate := 1.0
	bm := NewBucketManager(float64(capacity), refillRate)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 500

	t.Run("Concurrent Map Writes", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				_, _ = bm.Allow(ctx, ip)
			}(i)
		}
		wg.Wait()
	})

	t.Run("Concurrent Read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for range workers {
			wg.Go(func() {
				_, _ = bm.Allow(ctx, sharedIP)
			})
		}
		wg.Wait()
	})
}

// TestWindowManagerIsolation verifies that separate keys maintain independent
// sliding window state — exhausting one key's window must not affect another.
func TestWindowManagerIsolation(t *testing.T) {
	limit := int64(2)
	window := 1 * time.Second
	wm := NewWindowManager(limit, window)
	ctx := context.Background()

	ip1 := "192.168.1.100"
	ip2 := "10.0.0.5"

	wm.Allow(ctx, ip1)
	wm.Allow(ctx, ip1)

	allowed, err := wm.Allow(ctx, ip1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding limit", ip1)
	}

	allowed, err = wm.Allow(ctx, ip2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// TestWindowManagerConcurrency verifies that WindowManager is safe for concurrent
// use — covering both concurrent writes across distinct keys and concurrent reads
// on a single shared key.
func TestWindowManagerConcurrency(t *testing.T) {
	limit := int64(5)
	window := 1 * time.Second
	wm := NewWindowManager(limit, window)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 500

	t.Run("Concurrent Map Writes", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				_, _ = wm.Allow(ctx, ip)
			}(i)
		}
		wg.Wait()
	})

	t.Run("Concurrent Read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for range workers {
			wg.Go(func() {
				_, _ = wm.Allow(ctx, sharedIP)
			})
		}
		wg.Wait()
	})
}

// TestBucketManagerConcurrencyCorrectness verifies that BucketManager enforces
// rate limits correctly under concurrent load — all requests from distinct keys
// are allowed, and a single key is capped at its bucket capacity.
func TestBucketManagerConcurrencyCorrectness(t *testing.T) {
	capacity := float64(100)
	rate := float64(1)
	bm := NewBucketManager(capacity, rate)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 100

	var count atomic.Int64
	t.Run("Multiple IPs", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				isAllowed, err := bm.Allow(ctx, ip)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if isAllowed {
					count.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if count.Load() != int64(workers) {
			t.Errorf("expected all %d requests to be allowed, got %d", workers, count.Load())
		}
	})

	count.Store(0)
	extWorkers := 50
	singleIP := "192.168.1.100"
	t.Run("Single IP", func(t *testing.T) {
		for range workers + extWorkers {
			wg.Go(func() {
				isAllowed, err := bm.Allow(ctx, singleIP)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if isAllowed {
					count.Add(1)
				}
			})
		}
		wg.Wait()
		if count.Load() != int64(capacity) {
			t.Errorf("expected exactly %d requests to be allowed, got %d", int64(capacity), count.Load())
		}
	})
}

// TestWindowManagerConcurrencyCorrectness verifies that WindowManager enforces
// rate limits correctly under concurrent load — all requests from distinct keys
// are allowed, and a single key is capped at its window limit.
func TestWindowManagerConcurrencyCorrectness(t *testing.T) {
	limit := int64(100)
	window := 1 * time.Second
	wm := NewWindowManager(limit, window)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 100

	var count atomic.Int64
	t.Run("Multiple IPs", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				isAllowed, err := wm.Allow(ctx, ip)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if isAllowed {
					count.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if count.Load() != int64(workers) {
			t.Errorf("expected all %d requests to be allowed, got %d", workers, count.Load())
		}
	})

	count.Store(0)
	extWorkers := 50
	singleIP := "192.168.1.100"
	t.Run("Single IP", func(t *testing.T) {
		for range workers + extWorkers {
			wg.Go(func() {
				isAllowed, err := wm.Allow(ctx, singleIP)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if isAllowed {
					count.Add(1)
				}
			})
		}
		wg.Wait()
		if count.Load() != int64(limit) {
			t.Errorf("expected exactly %d requests to be allowed, got %d", int64(limit), count.Load())
		}
	})
}
