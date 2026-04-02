package limiter

import (
	"context"
	"sync"
	"time"
)

// SlidingWindow implements Limiter using the sliding window log algorithm.
// It tracks the timestamp of each allowed request in a fixed-size ring buffer,
// evicting expired entries on each call to Allow.
type SlidingWindow struct {
	mu     sync.Mutex
	log    *RingBuffer
	limit  int64
	window time.Duration
}

// NewSlidingWindow returns a SlidingWindow ready for use.
// limit is the maximum number of requests allowed per window.
// window is the duration of the sliding time window.
func NewSlidingWindow(limit int64, window time.Duration) *SlidingWindow {
	newSlidingWindow := SlidingWindow{
		log:    NewRingBuffer(limit),
		limit:  limit,
		window: window,
	}
	return &newSlidingWindow
}

// Allow returns true if the request is within the rate limit, false if it should
// be throttled. It evicts expired entries before checking the limit.
// It is safe for concurrent use.
func (sw *SlidingWindow) Allow(ctx context.Context, key string) (bool, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-sw.window)

	if sw.log.Count() < int(sw.limit) {
		sw.log.Push(now)
		return true, nil
	}
	if sw.log.Oldest().Before(cutoff) {
		sw.log.EvictOldest()
		sw.log.Push(now)
		return true, nil
	}
	return false, nil
}

// RingBuffer is a fixed-capacity circular buffer of timestamps.
// It is used by SlidingWindow to track request times with no heap allocations
// after construction.
type RingBuffer struct {
	data  []time.Time
	head  int
	tail  int
	count int
}

// NewRingBuffer returns a RingBuffer with the given capacity.
func NewRingBuffer(capacity int64) *RingBuffer {
	newRingBuffer := RingBuffer{
		data:  make([]time.Time, capacity),
		head:  0,
		tail:  0,
		count: 0,
	}
	return &newRingBuffer
}

// Push writes t at the tail of the buffer and advances the tail.
func (rb *RingBuffer) Push(t time.Time) {
	rb.data[rb.tail] = t
	rb.tail = (rb.tail + 1) % len(rb.data)
	rb.count += 1
}

// Oldest returns the timestamp at the head of the buffer without removing it.
func (rb *RingBuffer) Oldest() time.Time {
	return rb.data[rb.head]
}

// EvictOldest advances the head of the buffer, discarding the oldest entry.
func (rb *RingBuffer) EvictOldest() {
	rb.head = (rb.head + 1) % len(rb.data)
	rb.count -= 1
}

// Count returns the number of live entries in the buffer.
func (rb *RingBuffer) Count() int {
	return rb.count
}
