package limiter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Tests for BucketManager struct
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
		t.Errorf("unexpected error happened %s", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding limit", ip1)
	}

	allowed, err = bm.Allow(ctx, ip2)
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// Tests for BucketManager struct
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

	t.Run("Concurrent read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for _ = range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = bm.Allow(ctx, sharedIP)
			}()
		}
		wg.Wait()
	})
}

// Tests for WindowManager struct
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
		t.Errorf("unexpected error happened %s", err)
	}
	if allowed {
		t.Errorf("expected %s to be blocked after exceeding limit", ip1)
	}

	allowed, err = wm.Allow(ctx, ip2)
	if !allowed {
		t.Errorf("expected %s to be allowed, manager is leaking state between IPs", ip2)
	}
}

// Tests for WindowManager struct
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

	t.Run("Concurrent read from Single IP", func(t *testing.T) {
		sharedIP := "203.0.113.1"
		for _ = range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = wm.Allow(ctx, sharedIP)
			}()
		}
		wg.Wait()
	})
}
