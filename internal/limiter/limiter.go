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
