package limiter

import "context"

// Limiter is the common interface for all rate limiting algorithms.
type Limiter interface {
	// Allow returns true when key is within its configured limit.
	Allow(ctx context.Context, key string) (bool, error)
}
