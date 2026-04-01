// Package limiter provides rate limiting algorithms behind a common interface.
// Implementations include TokenBucket, with SlidingWindow and FixedWindow to follow.
package limiter

import "context"

// Limiter is the common interface for all rate limiting algorithms.
type Limiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}
