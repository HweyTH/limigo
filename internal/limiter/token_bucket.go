package limiter

import (
	"context"
	"sync"
	"time"
)

// TokenBucket implements Limiter using the token bucket algorithm.
// Tokens refill at a fixed rate up to a maximum capacity.
type TokenBucket struct {
	mu         sync.Mutex
	count      float64
	capacity   float64
	rate       float64
	lastRefill time.Time
}

// NewTokenBucket returns a TokenBucket initialised at full capacity.
// capacity is the maximum number of tokens the bucket can hold.
// rate is the number of tokens added per second.
func NewTokenBucket(capacity, rate float64) *TokenBucket {
	newBucket := TokenBucket{
		count:      capacity,
		capacity:   capacity,
		rate:       rate,
		lastRefill: time.Now(),
	}
	return &newBucket
}

// Allow consumes one token and returns true if the bucket had tokens available,
// or false if the bucket is empty and the request should be rate limited.
// It is safe for concurrent use.
func (bucket *TokenBucket) Allow(ctx context.Context, key string) (bool, error) {
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	elapsed := time.Since(bucket.lastRefill).Seconds()
	numAddTokens := elapsed * bucket.rate
	if bucket.count+numAddTokens < bucket.capacity {
		bucket.count += numAddTokens
	} else {
		bucket.count = bucket.capacity
	}
	bucket.lastRefill = time.Now()
	if bucket.count > 0 {
		bucket.count -= 1
		return true, nil
	}
	return false, nil
}
