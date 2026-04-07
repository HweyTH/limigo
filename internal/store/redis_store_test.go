package store

import (
	"context"
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
)

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
	fwScript, swScript, tbScript := scripts["fw"], scripts["sw"], scripts["tb"]
	store := NewRedisStore(globalRedisClient, fwScript, swScript, tbScript)
	ctx := context.Background()

	t.Run("Happy Path and Limit Enforcement", func(t *testing.T) {
		limit := 5
		window := 5 * time.Second
		key := "fixed-window-test-happy"

		for i := range limit {
			allowed, err := store.AllowFixedWindow(ctx, key, int64(limit), window)
			if err != nil {
				t.Fatalf("unexpected error on call %d: %v", i+1, err)
			}
			if !allowed {
				t.Fatalf("expected call %d to be allowed", i+1)
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

	t.Run("Concurrency", func(t *testing.T) {
		limit := 50
		window := 5 * time.Second
		var wg sync.WaitGroup
		var count atomic.Int64
		workers := 100
		for range workers {
			wg.Go(func() {
				allowed, err := store.AllowFixedWindow(ctx, "test-fixed-window-concurrent-key", int64(limit), window)
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

	t.Run("Key Expiration", func(t *testing.T) {
		//todo
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
	fwScript, swScript, tbScript := scripts["fw"], scripts["sw"], scripts["tb"]
	store := NewRedisStore(globalRedisClient, fwScript, swScript, tbScript)
	ctx := context.Background()

	t.Run("Happy Path and Drain", func(t *testing.T) {
		capacity := 10
		rate := 1
		key := "token-bucket-test-happy"

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
