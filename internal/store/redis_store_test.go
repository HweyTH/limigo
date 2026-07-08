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
	// scripts stores Lua source loaded once for RedisStore integration tests.
	scripts = make(map[string]string)
	// globalRedisClient is the Redis client shared by tests in this package.
	globalRedisClient *goredis.Client
	// redisKeyCounter makes generated Redis keys unique across subtests.
	redisKeyCounter atomic.Uint64
)

// newTestRedisStore returns a RedisStore using the process-wide testcontainer Redis.
func newTestRedisStore() *RedisStore {
	fwScript, swScript, tbScript := scripts["fw"], scripts["sw"], scripts["tb"]
	return NewRedisStore(globalRedisClient, fwScript, swScript, tbScript)
}

// redisTestKey returns a unique key per test case to avoid cross-test state leaks.
func redisTestKey(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("limigo:test:%s:%s:%d", t.Name(), suffix, redisKeyCounter.Add(1))
}

// init loads Lua sources once so tests exercise the same scripts embedded by RedisStore.
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

// TestMain provisions the shared Redis container used by store integration tests.
func TestMain(m *testing.M) {
	ctx := context.Background()

	// A single Redis container keeps the suite fast while per-test keys preserve isolation.
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

// TestAllowFixedWindow verifies fixed-window Redis behavior and failure handling.
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

// TestAllowSlidingWindow reserves coverage for sliding-window Redis behavior.
func TestAllowSlidingWindow(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	t.Run("HappyPathAllowsRequestsUpToLimit", func(t *testing.T) {
		limit := 2
		window := 5 * time.Second
		key := redisTestKey(t, "happy")

		for i := range limit {
			allowed, err := store.AllowSlidingWindow(ctx, key, int64(limit), window)
			if err != nil {
				t.Fatalf("unexpected error on call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected call %d to be allowed", i+1)
			}
		}
	})

	t.Run("UnhappyPathDeniesAfterLimitExceeded", func(t *testing.T) {
		limit := 2
		window := 5 * time.Second
		key := redisTestKey(t, "over-limit")

		for i := range limit {
			allowed, err := store.AllowSlidingWindow(ctx, key, int64(limit), window)
			if err != nil {
				t.Fatalf("unexpected error on setup call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected setup call %d to be allowed", i+1)
			}
		}

		allowed, err := store.AllowSlidingWindow(ctx, key, int64(limit), window)
		if err != nil {
			t.Fatalf("unexpected error on deny call: %v", err)
		}
		if allowed {
			t.Error("expected request to be denied after limit exceeded")
		}
	})

	t.Run("AllowsAgainAfterTheOldestRequestExpires", func(t *testing.T) {
		limit := int64(2)
		window := 100 * time.Millisecond
		key := redisTestKey(t, "oldest-expires")

		allowed, err := store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on setup call %d: %v", 1, err)
		}
		if !allowed {
			t.Fatalf("expected setup call %d to be allowed", 1)
		}

		time.Sleep(60 * time.Millisecond)

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on setup call %d: %v", 2, err)
		}
		if !allowed {
			t.Fatalf("expected setup call %d to be allowed", 2)
		}

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on setup call %d: %v", 3, err)
		}
		if allowed {
			t.Fatalf("expected setup call %d to be denied", 3)
		}

		time.Sleep(60 * time.Millisecond)

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on setup call %d: %v", 4, err)
		}
		if !allowed {
			t.Fatalf("expected setup call %d to be allowed", 4)
		}
	})

	t.Run("OnlyExpiredRequestsFreeCapacity", func(t *testing.T) {
		limit := int64(2)
		window := 100 * time.Millisecond
		key := redisTestKey(t, "partial-expiry")

		allowed, err := store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on setup call %d: %v", 1, err)
		}
		if !allowed {
			t.Fatalf("expected setup call %d to be allowed", 1)
		}

		time.Sleep(60 * time.Millisecond)

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on setup call %d: %v", 2, err)
		}
		if !allowed {
			t.Fatalf("expected setup call %d to be allowed", 2)
		}

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on deny call: %v", err)
		}
		if allowed {
			t.Fatalf("expected setup call %d to be denied", 3)
		}

		time.Sleep(60 * time.Millisecond)

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error after oldest request expired: %v", err)
		}
		if !allowed {
			t.Fatal("expected request to be allowed after oldest request expired")
		}

		allowed, err = store.AllowSlidingWindow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("unexpected error on final deny call: %v", err)
		}
		if allowed {
			t.Fatal("expected request to be denied because only one expired request freed capacity")
		}
	})
}

// TestAllowTokenBucket verifies token-bucket Redis behavior and planned edge cases.
func TestAllowTokenBucket(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	t.Run("HappyPathAndDrainFromBucket", func(t *testing.T) {
		capacity := float64(10)
		rate := float64(1)
		key := redisTestKey(t, "happy")

		for call := 1; call <= int(capacity); call++ {
			allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
			if err != nil {
				t.Fatalf("unexpected error on allowed call %d: %v", call, err)
			}
			if !allowed {
				t.Fatalf("expected call %d to be allowed while bucket still has tokens", call)
			}
		}

		allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on drained-bucket call: %v", err)
		}
		if allowed {
			t.Fatal("expected request to be denied after bucket was drained")
		}
	})

	t.Run("RefillAllowsAfterWaiting", func(t *testing.T) {
		capacity := float64(2)
		rate := float64(10)
		key := redisTestKey(t, "happy-refill")

		allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on allowed call %d: %v", 1, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed while bucket still has tokens", 1)
		}

		allowed, err = store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on allowed call %d: %v", 2, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed while bucket still has tokens", 2)
		}

		allowed, err = store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on drained-bucket call %d: %v", 3, err)
		}
		if allowed {
			t.Fatalf("expected call %d to be denied after bucket was drained", 3)
		}

		time.Sleep(120 * time.Millisecond)

		allowed, err = store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error after refill wait on call %d: %v", 4, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed after one token refilled", 4)
		}
	})

	t.Run("PartialRefillStillDenies", func(t *testing.T) {
		capacity := float64(1)
		rate := float64(10)
		key := redisTestKey(t, "partial-refill")

		allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on allowed call %d: %v", 1, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed while bucket still has tokens", 1)
		}

		allowed, err = store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on empty-bucket call %d: %v", 2, err)
		}
		if allowed {
			t.Fatalf("expected call %d to be denied because bucket has no tokens", 2)
		}

		time.Sleep(50 * time.Millisecond)

		allowed, err = store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error after partial refill on call %d: %v", 3, err)
		}
		if allowed {
			t.Fatalf("expected call %d to be denied because partial refill is less than one token", 3)
		}
	})

	t.Run("RefillNotAllowedPastCapacity", func(t *testing.T) {
		capacity := float64(2)
		rate := float64(10)
		key := redisTestKey(t, "refill-at-capacity")

		allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on allowed call %d: %v", 1, err)
		}
		if !allowed {
			t.Fatalf("expected call %d to be allowed while bucket still has tokens", 1)
		}

		time.Sleep(1000 * time.Millisecond)

		for call := 2; call <= int(capacity)+1; call++ {
			allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
			if err != nil {
				t.Fatalf("unexpected error on allowed call %d: %v", call, err)
			}
			if !allowed {
				t.Fatalf("expected call %d to be allowed while bucket has refilled capacity", call)
			}
		}
		allowed, err = store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error on over-capacity call %d: %v", 4, err)
		}
		if allowed {
			t.Fatalf("expected call %d to be denied because refill must not exceed bucket capacity", 4)
		}
	})
	t.Run("ConcurrentBurstAllowsOnlyCapacity", func(t *testing.T) {
		capacity := float64(50)
		rate := float64(1)
		workers := 100
		key := redisTestKey(t, "concurrent-burst")

		var allowedCount atomic.Int64
		var wg sync.WaitGroup

		for range workers {
			wg.Go(func() {
				allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
				if err != nil {
					t.Errorf("unexpected error during concurrent request: %v", err)
					return
				}
				if allowed {
					allowedCount.Add(1)
				}
			})
		}

		wg.Wait()

		if allowedCount.Load() != int64(capacity) {
			t.Fatalf("expected exactly %d requests to be allowed, got %d", int64(capacity), allowedCount.Load())
		}
	})

	t.Run("IndependentKeysDoNotShareBucket", func(t *testing.T) {
		capacity := float64(1)
		rate := float64(1)
		keyA := redisTestKey(t, "key-a")
		keyB := redisTestKey(t, "key-b")

		allowed, err := store.AllowTokenBucket(ctx, keyA, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error for first key: %v", err)
		}
		if !allowed {
			t.Fatal("expected first key to start with a full bucket")
		}

		allowed, err = store.AllowTokenBucket(ctx, keyA, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error for exhausted first key: %v", err)
		}
		if allowed {
			t.Fatal("expected second request for first key to be denied after its bucket was drained")
		}

		allowed, err = store.AllowTokenBucket(ctx, keyB, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error for second key: %v", err)
		}
		if !allowed {
			t.Fatal("expected second key to start with a full bucket")
		}

		allowed, err = store.AllowTokenBucket(ctx, keyB, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error for exhausted second key: %v", err)
		}
		if allowed {
			t.Fatal("expected second request for second key to be denied after its bucket was drained")
		}
	})

	t.Run("KeyExpiration", func(t *testing.T) {
		capacity := float64(1)
		rate := float64(1)
		key := redisTestKey(t, "expiration")

		allowed, err := store.AllowTokenBucket(ctx, key, capacity, rate)
		if err != nil {
			t.Fatalf("unexpected error while creating token bucket key: %v", err)
		}
		if !allowed {
			t.Fatal("expected first request to create a full token bucket and be allowed")
		}

		ttl, err := globalRedisClient.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("unexpected error while checking token bucket TTL: %v", err)
		}
		if ttl <= 0 {
			t.Fatalf("expected token bucket key to have a positive TTL, got %s", ttl)
		}
		if ttl > time.Minute {
			t.Fatalf("expected token bucket TTL to be capped at one minute for this rate, got %s", ttl)
		}
	})
}

// TestDisconnectedClient verifies Redis-backed limiters fail closed when Redis is unavailable.
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
