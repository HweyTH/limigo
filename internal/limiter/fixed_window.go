package limiter

import (
	"context"
	"sync"
	"time"
)

// FixedWindow implements Limiter using the fixed window counter algorithm.
// It counts requests against a single time window and resets the count once
// the window duration has elapsed.
type FixedWindow struct {
	mu          sync.Mutex
	limit       int64
	window      time.Duration
	count       int64
	windowStart time.Time
}

// NewFixedWindow returns a FixedWindow ready for use.
// limit is the maximum number of requests allowed per window.
// window is the duration before the counter resets.
func NewFixedWindow(limit int64, window time.Duration) *FixedWindow {
	newFxWindow := FixedWindow{
		limit:       limit,
		window:      window,
		windowStart: time.Now(),
	}
	return &newFxWindow
}

// Allow returns true if the request is within the current fixed window limit,
// false if the request should be throttled. It resets the window lazily on the
// first request after the configured window duration has elapsed.
// It is safe for concurrent use.
func (fw *FixedWindow) Allow(ctx context.Context, key string) (bool, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if time.Since(fw.windowStart) > fw.window {
		fw.count = 0
		fw.windowStart = time.Now()
	}
	if fw.count < fw.limit {
		fw.count += 1
		return true, nil
	}
	return false, nil
}
