package limiter

import (
	"context"
	"sync"
	"time"
)

type FixedWindow struct {
	mu          sync.Mutex
	limit       int64
	window      time.Duration
	count       int64
	windowStart time.Time
}

func NewFixedWindow(limit int64, window time.Duration) *FixedWindow {
	newFxWindow := FixedWindow{
		limit:       limit,
		window:      window,
		windowStart: time.Now(),
	}
	return &newFxWindow
}

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
