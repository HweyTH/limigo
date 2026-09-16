package limiter

import (
	"context"
	"math"
	"sync"
)

// BatchingTokenBucket implements node-local burst absorption for the token
// bucket algorithm. Allow decides against a local baseline — the last known
// authoritative token count minus any local admits not yet reconciled — with
// zero network round-trip. Unlike the backing store's own refill logic, the
// local baseline does not simulate refill between flushes: it is a snapshot
// updated only when ApplyRemoteTotal runs, which is a deliberate part of the
// accuracy trade-off — refill accuracy is deferred entirely to the backing
// store, keeping the local decision path simple.
type BatchingTokenBucket struct {
	mu           sync.Mutex
	capacity     float64
	rate         float64
	remoteTokens float64
	pendingDelta float64
}

// NewBatchingTokenBucket returns a BatchingTokenBucket initialized at full
// capacity. capacity is the maximum number of tokens the bucket can hold.
// rate is the number of tokens added per second by the backing store.
func NewBatchingTokenBucket(capacity, rate float64) *BatchingTokenBucket {
	return &BatchingTokenBucket{capacity: capacity, rate: rate, remoteTokens: capacity}
}

// Allow returns true if a token is available, decided against the last known
// remote token count minus any local admits not yet reconciled with the
// backing store. It never contacts a backing store and is safe for
// concurrent use.
func (b *BatchingTokenBucket) Allow(ctx context.Context, key string) (bool, error) {
	allowed, _ := b.Admit(ctx, key)
	return allowed, nil
}

// Admit is Allow with the node's local view of the whole tokens remaining
// after the decision: the last known remote count minus every local admit
// not yet reconciled, floored and clamped at zero. It is a local view, not
// the fleet's, and is read in the same critical section as the decision.
func (b *BatchingTokenBucket) Admit(ctx context.Context, key string) (allowed bool, remaining int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remoteTokens-b.pendingDelta >= 1 {
		b.pendingDelta++
		allowed = true
	}
	local := b.remoteTokens - b.pendingDelta
	if local < 0 {
		local = 0
	}
	return allowed, int64(math.Floor(local))
}

// PendingDelta returns the number of tokens consumed locally and not yet
// reconciled with the backing store, for a caller to flush.
func (b *BatchingTokenBucket) PendingDelta() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingDelta
}

// Exhausted reports whether the local baseline currently has no token to give,
// so Allow is denying every request.
//
// A caller's flush cycle must reconcile an exhausted bucket even when it has
// no pending delta to write. An exhausted bucket admits nothing, so it accrues
// no delta, so a flush cycle that skipped zero-delta buckets would never call
// the backing store for it again — and the baseline is only ever refreshed by
// a flush. It would deny every request for the remaining life of the process,
// however much headroom the store had refilled in the meantime.
func (b *BatchingTokenBucket) Exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remoteTokens-b.pendingDelta < 1
}

// FillRatio returns the bucket's local view of how full it is, as a value
// clamped to [0,1]: (remoteTokens - pendingDelta) / capacity. A non-positive
// capacity returns 0 rather than dividing by zero.
func (b *BatchingTokenBucket) FillRatio() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capacity <= 0 {
		return 0
	}
	ratio := (b.remoteTokens - b.pendingDelta) / b.capacity
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

// ApplyRemoteTotal reconciles a successful flush. flushed is the delta value
// that was just synced to the backing store (the value PendingDelta returned
// before the flush began), and remaining is the authoritative token count the
// backing store reported afterward, which can be negative if the fleet
// over-admitted relative to the true bucket state during the interval since
// the last sync. Subtracting exactly flushed — rather than resetting the
// pending delta to zero — keeps any admits that arrived while the flush was
// in flight correctly reflected against the new baseline.
func (b *BatchingTokenBucket) ApplyRemoteTotal(flushed float64, remaining float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingDelta -= flushed
	b.remoteTokens = remaining
}
