package limiter

import (
	"context"
	"sync"
)

// BucketManager manages a pool of TokenBuckets, one per key.
// Buckets are created on demand and routed by key on each Allow call.
// All operations are safe for concurrent use.
type BucketManager struct {
	mu       sync.RWMutex
	buckets  map[string]*TokenBucket
	capacity float64
	rate     float64
}

// NewBucketManager returns a BucketManager ready for use.
// capacity and rate are passed to each TokenBucket created on demand —
// see NewTokenBucket for their semantics.
func NewBucketManager(capacity, rate float64) *BucketManager {
	newBucketManager := BucketManager{
		buckets:  make(map[string]*TokenBucket),
		capacity: capacity,
		rate:     rate,
	}
	return &newBucketManager
}

// Allow returns true if the given key is within its rate limit, false if it should
// be throttled. A bucket is created for the key if one does not already exist.
// It uses double-checked locking to minimise contention on the common path.
func (manager *BucketManager) Allow(ctx context.Context, key string) (bool, error) {
	manager.mu.RLock()
	bucket, exists := manager.buckets[key]
	manager.mu.RUnlock()
	if exists {
		return bucket.Allow(ctx, key)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if bucket, exists := manager.buckets[key]; !exists {
		newBucket := NewTokenBucket(manager.capacity, manager.rate)
		manager.buckets[key] = newBucket
		return newBucket.Allow(ctx, key)
	} else {
		return bucket.Allow(ctx, key)
	}
}
