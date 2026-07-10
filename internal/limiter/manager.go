package limiter

import (
	"context"
	"maps"
	"sync"
	"time"
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

// WindowManager manages a pool of SlidingWindows, one per key.
// Windows are created on demand and routed by key on each Allow call.
// All operations are safe for concurrent use.
type WindowManager struct {
	mu      sync.RWMutex
	clients map[string]*SlidingWindow
	limit   int64
	window  time.Duration
}

// NewWindowManager returns a WindowManager ready for use.
// limit and window are passed to each SlidingWindow created on demand —
// see NewSlidingWindow for their semantics.
func NewWindowManager(limit int64, window time.Duration) *WindowManager {
	wm := WindowManager{
		clients: make(map[string]*SlidingWindow),
		limit:   limit,
		window:  window,
	}
	return &wm
}

// Allow returns true if the given key is within its rate limit, false if it should
// be throttled. A window is created for the key if one does not already exist.
// It uses double-checked locking to minimise contention on the common path.
func (manager *WindowManager) Allow(ctx context.Context, key string) (bool, error) {
	manager.mu.RLock()
	sw, exists := manager.clients[key]
	manager.mu.RUnlock()
	if exists {
		return sw.Allow(ctx, key)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if sw, exists := manager.clients[key]; !exists {
		newWindow := NewSlidingWindow(manager.limit, manager.window)
		manager.clients[key] = newWindow
		return newWindow.Allow(ctx, key)
	} else {
		return sw.Allow(ctx, key)
	}
}

// BatchingFixedWindowManager manages a pool of BatchingFixedWindows, one per key.
// Windows are created on demand and routed by key on each Allow call.
// All operations are safe for concurrent use.
type BatchingFixedWindowManager struct {
	mu      sync.RWMutex
	windows map[string]*BatchingFixedWindow
	limit   int64
}

// NewBatchingFixedWindowManager returns a BatchingFixedWindowManager ready for use.
// limit is passed to each BatchingFixedWindow created on demand — see
// NewBatchingFixedWindow for its semantics.
func NewBatchingFixedWindowManager(limit int64) *BatchingFixedWindowManager {
	return &BatchingFixedWindowManager{
		windows: make(map[string]*BatchingFixedWindow),
		limit:   limit,
	}
}

// Allow returns true if the given key is within its rate limit, false if it should
// be throttled. A window is created for the key if one does not already exist.
// It uses double-checked locking to minimise contention on the common path.
func (manager *BatchingFixedWindowManager) Allow(ctx context.Context, key string) (bool, error) {
	manager.mu.RLock()
	w, exists := manager.windows[key]
	manager.mu.RUnlock()
	if exists {
		return w.Allow(ctx, key)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if w, exists := manager.windows[key]; !exists {
		newWindow := NewBatchingFixedWindow(manager.limit)
		manager.windows[key] = newWindow
		return newWindow.Allow(ctx, key)
	} else {
		return w.Allow(ctx, key)
	}
}

// Snapshot returns a copy of all per-key BatchingFixedWindows currently
// tracked, for a caller to flush each against a backing store.
func (manager *BatchingFixedWindowManager) Snapshot() map[string]*BatchingFixedWindow {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	snapshot := make(map[string]*BatchingFixedWindow, len(manager.windows))
	maps.Copy(snapshot, manager.windows)
	return snapshot
}

// BatchingTokenBucketManager manages a pool of BatchingTokenBuckets, one per key.
// Buckets are created on demand and routed by key on each Allow call.
// All operations are safe for concurrent use.
type BatchingTokenBucketManager struct {
	mu       sync.RWMutex
	buckets  map[string]*BatchingTokenBucket
	capacity float64
	rate     float64
}

// NewBatchingTokenBucketManager returns a BatchingTokenBucketManager ready for use.
// capacity and rate are passed to each BatchingTokenBucket created on demand —
// see NewBatchingTokenBucket for their semantics.
func NewBatchingTokenBucketManager(capacity, rate float64) *BatchingTokenBucketManager {
	return &BatchingTokenBucketManager{
		buckets:  make(map[string]*BatchingTokenBucket),
		capacity: capacity,
		rate:     rate,
	}
}

// Allow returns true if the given key is within its rate limit, false if it should
// be throttled. A bucket is created for the key if one does not already exist.
// It uses double-checked locking to minimise contention on the common path.
func (manager *BatchingTokenBucketManager) Allow(ctx context.Context, key string) (bool, error) {
	manager.mu.RLock()
	bucket, exists := manager.buckets[key]
	manager.mu.RUnlock()
	if exists {
		return bucket.Allow(ctx, key)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if bucket, exists := manager.buckets[key]; !exists {
		newBucket := NewBatchingTokenBucket(manager.capacity, manager.rate)
		manager.buckets[key] = newBucket
		return newBucket.Allow(ctx, key)
	} else {
		return bucket.Allow(ctx, key)
	}
}

// Snapshot returns a copy of all per-key BatchingTokenBuckets currently
// tracked, for a caller to flush each against a backing store.
func (manager *BatchingTokenBucketManager) Snapshot() map[string]*BatchingTokenBucket {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	snapshot := make(map[string]*BatchingTokenBucket, len(manager.buckets))
	maps.Copy(snapshot, manager.buckets)
	return snapshot
}
