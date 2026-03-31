package limiter

import (
	"context"
	"time"
)

// Limiter is the common interface for all rate limiting algorithms.
type Limiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}

type TokenBucket struct {
	count      float64
	capacity   float64
	rate       float64
	lastRefill time.Time
}

func (bucket *TokenBucket) Allow(ctx context.Context, key string) (bool, error) {
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
