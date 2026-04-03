package store

import (
	"github.com/redis/go-redis/v9"
)

// RedisStore is a Redis-backed implementation of FixedWindowStore, SlidingWindowStore,
// and TokenBucketStore. Each algorithm's logic runs inside a Lua script executed
// atomically on the Redis server to prevent race conditions across nodes.
type RedisStore struct {
	client              *redis.Client
	fixedWindowScript   *redis.Script
	slidingWindowScript *redis.Script
	tokenBucketScript   *redis.Script
}

// NewRedisStore returns a RedisStore backed by the given Redis client.
// fwScript, swScript, and tbScript are the Lua source strings for the fixed window,
// sliding window, and token bucket algorithms respectively.
func NewRedisStore(redisClient *redis.Client, fwScript string, swScript string, tbScript string) *RedisStore {
	newRedisStore := RedisStore{
		client:              redisClient,
		fixedWindowScript:   redis.NewScript(fwScript),
		slidingWindowScript: redis.NewScript(swScript),
		tokenBucketScript:   redis.NewScript(tbScript),
	}
	return &newRedisStore
}
