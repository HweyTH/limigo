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

// TestLeakyBucketManagerIsolation verifies that separate keys maintain
// independent GCRA schedules — exhausting one key's burst tolerance must not
// affect another.
func TestLeakyBucketManagerIsolation(t *testing.T) {
	limit := int64(10)
	window := 100 * time.Millisecond
	burst := int64(2)
	lbm := NewLeakyBucketManager(limit, window, burst)
	ctx := context.Background()

	ip1 := "192.168.1.100"
	ip2 := "10.0.0.5"

	lbm.Allow(ctx, ip1)
	lbm.Allow(ctx, ip1)

	allowed, err := lbm.Allow(ctx, ip1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding burst tolerance", ip1)
	}

	allowed, err = lbm.Allow(ctx, ip2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// TestLeakyBucketManagerConcurrency verifies that LeakyBucketManager is safe
// for concurrent use — covering both concurrent writes across distinct keys
// and concurrent reads on a single shared key.
func TestLeakyBucketManagerConcurrency(t *testing.T) {
	limit := int64(5)
	window := 1 * time.Second
	lbm := NewLeakyBucketManager(limit, window, 1)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 500

	t.Run("Concurrent Map Writes", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				_, _ = lbm.Allow(ctx, ip)
			}(i)
		}
		wg.Wait()
	})

	t.Run("Concurrent Read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for range workers {
			wg.Go(func() {
				_, _ = lbm.Allow(ctx, sharedIP)
			})
		}
		wg.Wait()
	})
}

// TestBatchingFixedWindowManagerIsolation verifies that separate keys maintain
// independent batching fixed window state — exhausting one key's local limit
// must not affect another.
func TestBatchingFixedWindowManagerIsolation(t *testing.T) {
	limit := int64(2)
	bwm := NewBatchingFixedWindowManager(limit)
	ctx := context.Background()

	ip1 := "192.168.1.100"
	ip2 := "10.0.0.5"

	bwm.Allow(ctx, ip1)
	bwm.Allow(ctx, ip1)

	allowed, err := bwm.Allow(ctx, ip1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding limit", ip1)
	}

	allowed, err = bwm.Allow(ctx, ip2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// TestBatchingFixedWindowManagerConcurrency verifies that
// BatchingFixedWindowManager is safe for concurrent use — covering both
// concurrent writes across distinct keys and concurrent reads on a single
// shared key.
func TestBatchingFixedWindowManagerConcurrency(t *testing.T) {
	limit := int64(5)
	bwm := NewBatchingFixedWindowManager(limit)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 500

	t.Run("Concurrent Map Writes", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				_, _ = bwm.Allow(ctx, ip)
			}(i)
		}
		wg.Wait()
	})

	t.Run("Concurrent Read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for range workers {
			wg.Go(func() {
				_, _ = bwm.Allow(ctx, sharedIP)
			})
		}
		wg.Wait()
	})
}

// TestBatchingFixedWindowManagerSnapshotIsolation verifies Snapshot returns an
// independent copy — later map mutations on either side must not affect the
// other, since a flush cycle (task #7) will iterate a snapshot while Allow
// continues to run concurrently and may create new per-key windows.
func TestBatchingFixedWindowManagerSnapshotIsolation(t *testing.T) {
	bwm := NewBatchingFixedWindowManager(10)
	ctx := context.Background()

	if _, err := bwm.Allow(ctx, "existing-key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snapshot := bwm.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("len(snapshot) = %d, want 1", len(snapshot))
	}

	if _, err := bwm.Allow(ctx, "new-key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshot) != 1 {
		t.Fatalf("snapshot mutated after manager gained a new key: len = %d, want 1", len(snapshot))
	}

	delete(snapshot, "existing-key")
	if _, ok := bwm.Snapshot()["existing-key"]; !ok {
		t.Fatal("mutating a returned snapshot must not affect the manager's internal state")
	}
}

// TestBatchingTokenBucketManagerIsolation verifies that separate keys maintain
// independent batching token bucket state — exhausting one key's local
// capacity must not affect another.
func TestBatchingTokenBucketManagerIsolation(t *testing.T) {
	capacity := float64(2)
	rate := float64(1)
	btm := NewBatchingTokenBucketManager(capacity, rate)
	ctx := context.Background()

	ip1 := "192.168.1.100"
	ip2 := "10.0.0.5"

	btm.Allow(ctx, ip1)
	btm.Allow(ctx, ip1)

	allowed, err := btm.Allow(ctx, ip1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding capacity", ip1)
	}

	allowed, err = btm.Allow(ctx, ip2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// TestBatchingTokenBucketManagerConcurrency verifies that
// BatchingTokenBucketManager is safe for concurrent use — covering both
// concurrent writes across distinct keys and concurrent reads on a single
// shared key.
func TestBatchingTokenBucketManagerConcurrency(t *testing.T) {
	capacity := float64(2)
	rate := float64(1)
	btm := NewBatchingTokenBucketManager(capacity, rate)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 500

	t.Run("Concurrent Map Writes", func(t *testing.T) {
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ip := fmt.Sprintf("192.168.1.%d", id)
				_, _ = btm.Allow(ctx, ip)
			}(i)
		}
		wg.Wait()
	})

	t.Run("Concurrent Read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for range workers {
			wg.Go(func() {
				_, _ = btm.Allow(ctx, sharedIP)
			})
		}
		wg.Wait()
	})
}

// TestBatchingTokenBucketManagerSnapshotIsolation verifies Snapshot returns an
// independent copy — later map mutations on either side must not affect the
// other, since a flush cycle (task #7) will iterate a snapshot while Allow
// continues to run concurrently and may create new per-key buckets.
func TestBatchingTokenBucketManagerSnapshotIsolation(t *testing.T) {
	btm := NewBatchingTokenBucketManager(10, 1)
	ctx := context.Background()

	if _, err := btm.Allow(ctx, "existing-key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snapshot := btm.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("len(snapshot) = %d, want 1", len(snapshot))
	}

	if _, err := btm.Allow(ctx, "new-key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshot) != 1 {
		t.Fatalf("snapshot mutated after manager gained a new key: len = %d, want 1", len(snapshot))
	}

	delete(snapshot, "existing-key")
	if _, ok := btm.Snapshot()["existing-key"]; !ok {
		t.Fatal("mutating a returned snapshot must not affect the manager's internal state")
	}
}
