package limiter

import (
	"context"
	"sync"
	"time"
)

// LeakyBucket implements Limiter using the leaky bucket algorithm in its GCRA
// (Generic Cell Rate Algorithm) form. Instead of tracking a token count, it
// tracks a single Theoretical Arrival Time (TAT) — the earliest time the next
// request is due under a perfectly smooth schedule — and admits a request if
// it arrives no earlier than the configured tolerance before that time.
type LeakyBucket struct {
	mu               sync.Mutex
	tat              time.Time
	emissionInterval time.Duration
	tolerance        time.Duration
}

// NewLeakyBucket returns a LeakyBucket ready for immediate admission.
// limit and window define the emission interval — the ideal spacing between
// admitted requests — as window / limit. burst is the number of requests
// allowed to arrive back-to-back before subsequent requests must wait for the
// schedule to catch up; values less than 1 are treated as 1 (no clumping
// tolerance).
func NewLeakyBucket(limit int64, window time.Duration, burst int64) *LeakyBucket {
	if burst < 1 {
		burst = 1
	}
	emissionInterval := window / time.Duration(limit)
	newBucket := LeakyBucket{
		tat:              time.Now(),
		emissionInterval: emissionInterval,
		tolerance:        emissionInterval * time.Duration(burst-1),
	}
	return &newBucket
}

// Allow returns true if the request arrives on schedule, false if it arrives
// too early and should be throttled. Allowing a request advances the
// schedule by one emission interval; denying one leaves it untouched, so a
// rejected burst does not push future requests further out than the
// configured rate allows. It is safe for concurrent use.
func (bucket *LeakyBucket) Allow(ctx context.Context, key string) (bool, error) {
	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	now := time.Now()
	tat := bucket.tat
	if tat.Before(now) {
		tat = now
	}

	allowAt := tat.Add(-bucket.tolerance)
	if now.Before(allowAt) {
		return false, nil
	}

	bucket.tat = tat.Add(bucket.emissionInterval)
	return true, nil
}
