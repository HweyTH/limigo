package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

var (
	scripts           = make(map[string]string)
	globalRedisClient *goredis.Client
	redisKeyCounter   atomic.Uint64
)

func newTestRedisStore() *RedisStore {
	fwScript, swScript, tbScript := scripts["fw"], scripts["sw"], scripts["tb"]
	return NewRedisStore(globalRedisClient, fwScript, swScript, tbScript)
}

func redisTestKey(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("limigo:test:%s:%s:%d", t.Name(), suffix, redisKeyCounter.Add(1))
}

func init() {
	files := map[string]string{
		"fw": "lua/fixed_window.lua",
		"sw": "lua/sliding_window.lua",
		"tb": "lua/token_bucket.lua",
	}
	for name, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			panic("failed to load lua script: " + path)
		}
		scripts[name] = string(content)
	}
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic("failed to start container: " + err.Error())
	}

	endpoint, err := redisContainer.Endpoint(ctx, "")
	if err != nil {
		panic("failed to get container endpoint: " + err.Error())
	}

	globalRedisClient = goredis.NewClient(&goredis.Options{
		Addr: endpoint,
	})

	code := m.Run()

	globalRedisClient.Close()
	redisContainer.Terminate(ctx)

	os.Exit(code)
}

func TestAllowFixedWindow(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	t.Run("HappyPathAllowsRequestsUpToLimit", func(t *testing.T) {
		limit := 5
		window := 5 * time.Second
		key := redisTestKey(t, "happy")

		for i := range limit {
			allowed, err := store.AllowFixedWindow(ctx, key, int64(limit), window)
			if err != nil {
				t.Fatalf("unexpected error on call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected call %d to be allowed", i+1)
			}
		}
	})

	t.Run("UnhappyPathDeniesAfterLimitExceeded", func(t *testing.T) {
		limit := 3
		window := 5 * time.Second
		key := redisTestKey(t, "over-limit")

		for i := range limit {
			allowed, err := store.AllowFixedWindow(ctx, key, int64(limit), window)
			if err != nil {
				t.Fatalf("unexpected error on setup call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected setup call %d to be allowed", i+1)
			}
		}

		allowed, err := store.AllowFixedWindow(ctx, key, int64(limit), window)
		if err != nil {
			t.Fatalf("unexpected error on deny call: %v", err)
		}
		if allowed {
			t.Error("expected request to be denied after limit exceeded")
		}
	})

	t.Run("UnhappyPathReturnsErrorAndFailsClosedWhenContextIsCanceled", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()

		allowed, err := store.AllowFixedWindow(canceledCtx, redisTestKey(t, "canceled"), 1, time.Second)
		if err == nil {
			t.Fatal("expected context cancellation error, got nil")
		}
		if allowed {
			t.Fatal("expected canceled request to fail closed")
		}
	})

	t.Run("EdgeCaseLimitOneAllowsOnlyFirstRequest", func(t *testing.T) {
		key := redisTestKey(t, "limit-one")

		allowed, err := store.AllowFixedWindow(ctx, key, 1, time.Second)
		if err != nil {
			t.Fatalf("unexpected error on first call: %v", err)
		}
		if !allowed {
			t.Fatal("expected first request to be allowed when limit is one")
		}

		allowed, err = store.AllowFixedWindow(ctx, key, 1, time.Second)
		if err != nil {
			t.Fatalf("unexpected error on second call: %v", err)
		}
		if allowed {
			t.Fatal("expected second request to be denied when limit is one")
		}
	})

	t.Run("EdgeCaseZeroLimitDeniesAllRequests", func(t *testing.T) {
		key := redisTestKey(t, "zero-limit")

		for i := range 3 {
			allowed, err := store.AllowFixedWindow(ctx, key, 0, time.Second)
			if err != nil {
				t.Fatalf("unexpected error on call %d: %v", i+1, err)
			}
			if allowed {
				t.Fatalf("expected call %d to be denied when limit is zero", i+1)
			}
		}
	})

	t.Run("EdgeCaseWindowExpirationResetsQuota", func(t *testing.T) {
		limit := int64(2)
		window := 75 * time.Millisecond
		key := redisTestKey(t, "expiration")

		for i := range limit {
			allowed, err := store.AllowFixedWindow(ctx, key, limit, window)
			if err != nil {
				t.Fatalf("unexpected error on setup call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected setup call %d to be allowed", i+1)
			}
		}

		allowed, err := store.AllowFixedWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on over-limit call: %v", err)
		}
		if allowed {
			t.Fatal("expected request to be denied before the window expires")
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			ttl, err := globalRedisClient.PTTL(ctx, key).Result()
			if err != nil {
				t.Fatalf("unexpected error while checking TTL: %v", err)
			}
			if ttl == -2*time.Nanosecond {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		ttl, err := globalRedisClient.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("unexpected error while checking final TTL: %v", err)
		}
		if ttl != -2*time.Nanosecond {
			t.Fatalf("expected fixed-window key to expire, got TTL %s", ttl)
		}

		allowed, err = store.AllowFixedWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error after window reset: %v", err)
		}
		if !allowed {
			t.Fatal("expected request to be allowed after the window expires")
		}
	})

	t.Run("ConcurrencyRequestsFromManyIPsAreIsolated", func(t *testing.T) {
		limit := int64(1)
		window := 5 * time.Second
		workers := 100
		var wg sync.WaitGroup
		var allowedCount atomic.Int64

		for i := range workers {
			ip := fmt.Sprintf("198.51.100.%d", i)
			key := redisTestKey(t, ip)

			wg.Go(func() {
				allowed, err := store.AllowFixedWindow(ctx, key, limit, window)
				if err != nil {
					t.Errorf("unexpected error for key %q: %v", key, err)
					return
				}
				if allowed {
					allowedCount.Add(1)
				}
			})
		}
		wg.Wait()

		if got := allowedCount.Load(); got != int64(workers) {
			t.Fatalf("expected all %d distinct IP requests to be allowed, got %d", workers, got)
		}
	})

	t.Run("ConcurrencySharedIPIsCappedAtomically", func(t *testing.T) {
		limit := 50
		window := 5 * time.Second
		var wg sync.WaitGroup
		var count atomic.Int64
		workers := 100
		key := redisTestKey(t, "shared-ip")

		for range workers {
			wg.Go(func() {
				allowed, err := store.AllowFixedWindow(ctx, key, int64(limit), window)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if allowed {
					count.Add(1)
				}
			})
		}
		wg.Wait()
		if count.Load() != int64(limit) {
			t.Errorf("expected %d requests to come through, but got %d", limit, count.Load())
		}
	})

	t.Run("ManyConsecutiveRequestsFromOneIP", func(t *testing.T) {
		limit := 25
		totalRequests := 40
		window := 5 * time.Second
		key := redisTestKey(t, "single-ip-sequential")
		var allowedCount int
		var deniedCount int

		for i := range totalRequests {
			allowed, err := store.AllowFixedWindow(ctx, key, int64(limit), window)
			if err != nil {
				t.Fatalf("unexpected error on call %d: %v", i+1, err)
			}
			if allowed {
				allowedCount++
			} else {
				deniedCount++
			}
		}

		if allowedCount != limit {
			t.Fatalf("expected exactly %d allowed requests, got %d", limit, allowedCount)
		}
		if deniedCount != totalRequests-limit {
			t.Fatalf("expected exactly %d denied requests, got %d", totalRequests-limit, deniedCount)
		}
	})
}

func TestAllowSlidingWindow(t *testing.T) {
	// fwScript, swScript, tbScript := scripts["fw"], scripts["sw"], scripts["tb"]
	// store := NewRedisStore(globalRedisClient, fwScript, swScript, tbScript)
	// // ctx := context.Background()

	// t.Run("Happy Path and Limit Enforcement", func(t *testing.T) {
	// 	// TODO: Implement sliding window limit enforcement test
	// })

	// t.Run("Rolling Expiration", func(t *testing.T) {
	// 	// TODO: Implement test to verify older requests fall out of the time window
	// })

	// t.Run("Concurrency", func(t *testing.T) {
	// 	// TODO: Implement Thundering Herd concurrency test for sliding window
	// })

	// t.Run("Key Expiration", func(t *testing.T) {
	// 	// TODO: Implement TTL verification test for sliding window
	// })
}

func TestAllowTokenBucket(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	t.Run("Happy Path and Drain", func(t *testing.T) {
		capacity := 10
		rate := 1
		key := redisTestKey(t, "happy")

		for i := 0; i < capacity; i++ {
			allowed, err := store.AllowTokenBucket(ctx, key, float64(capacity), float64(rate))
			if err != nil {
				t.Fatalf("unexpected error on call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected call %d to be allowed", i+1)
			}
		}

		allowed, err := store.AllowTokenBucket(ctx, key, float64(capacity), float64(rate))
		if err != nil {
			t.Fatalf("unexpected error on deny call: %v", err)
		}
		if allowed {
			t.Error("expected request to be denied after limit exceeded")
		}
	})

	t.Run("Refill Logic", func(t *testing.T) {
		// TODO: Implement test with time.Sleep or mock clock to verify bucket refills correctly
	})

	t.Run("Concurrency", func(t *testing.T) {
		// TODO: Implement Thundering Herd concurrency test for token bucket
	})

	t.Run("Key Expiration", func(t *testing.T) {
		// TODO: Implement TTL verification test for token bucket
	})
}

func TestDisconnectedClient(t *testing.T) {
	badClient := goredis.NewClient(&goredis.Options{
		Addr: "localhost:12334667", // Intentionally unreachable
	})

	fwScript, swScript, tbScript := scripts["fw"], scripts["sw"], scripts["tb"]
	store := NewRedisStore(badClient, fwScript, swScript, tbScript)
	ctx := context.Background()

	t.Run("Fixed Window Fail Closed", func(t *testing.T) {
		success, err := store.AllowFixedWindow(ctx, "test-key-fw", 10, time.Millisecond)
		if err == nil {
			t.Errorf("Expected an error due to disconnected Redis, but got nil")
		}
		if success {
			t.Errorf("Expected success to be false due to bad Redis client")
		}
	})

	t.Run("Sliding Window Fail Closed", func(t *testing.T) {
		success, err := store.AllowSlidingWindow(ctx, "test-key-sw", 10, time.Millisecond)
		if err == nil {
			t.Errorf("Expected an error due to disconnected Redis, but got nil")
		}
		if success {
			t.Errorf("Expected success to be false due to bad Redis client")
		}
	})

	t.Run("Token Bucket Fail Closed", func(t *testing.T) {
		success, err := store.AllowTokenBucket(ctx, "test-key-tb", float64(10), float64(1))
		if err == nil {
			t.Errorf("Expected an error due to disconnected Redis, but got nil")
		}
		if success {
			t.Errorf("Expected success to be false due to bad Redis client")
		}
	})
}
