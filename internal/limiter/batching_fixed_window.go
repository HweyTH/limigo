package limiter

import (
	"context"
	"sync"
)

// BatchingFixedWindow implements node-local burst absorption for the fixed
// window algorithm. Allow decides against a local baseline — the last known
// authoritative total plus any local admits not yet reconciled — with zero
// network round-trip. The pending delta is periodically reconciled with a
// backing store by a caller-driven flush cycle (see PendingDelta and
// ApplyRemoteTotal), trading a small accuracy window for lower request
// latency and reduced load on the backing store.
type BatchingFixedWindow struct {
	mu           sync.Mutex
	limit        int64
	remoteTotal  int64
	pendingDelta int64
}

// NewBatchingFixedWindow returns a BatchingFixedWindow ready for use.
// limit is the maximum number of requests allowed per window.
func NewBatchingFixedWindow(limit int64) *BatchingFixedWindow {
	return &BatchingFixedWindow{limit: limit}
}

// Allow returns true if the request is within limit, decided against the last
// known remote total plus any local admits not yet reconciled with the
// backing store. It never contacts a backing store and is safe for
// concurrent use.
func (b *BatchingFixedWindow) Allow(ctx context.Context, key string) (bool, error) {
	allowed, _ := b.Admit(ctx, key)
	return allowed, nil
}

// Admit is Allow with the node's local view of the remaining quota after the
// decision: limit minus the last known remote total minus every local admit
// not yet reconciled, clamped at zero. It is a local view, not the fleet's —
// other nodes' unflushed admits are invisible here — and it is read in the
// same critical section as the decision so the two cannot disagree.
func (b *BatchingFixedWindow) Admit(ctx context.Context, key string) (allowed bool, remaining int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remoteTotal+b.pendingDelta < b.limit {
		b.pendingDelta++
		allowed = true
	}
	remaining = b.limit - (b.remoteTotal + b.pendingDelta)
	if remaining < 0 {
		remaining = 0
	}
	return allowed, remaining
}

// PendingDelta returns the number of local admits not yet reconciled with the
// backing store, for a caller to flush.
func (b *BatchingFixedWindow) PendingDelta() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingDelta
}

// Exhausted reports whether the local baseline has reached the limit, so Allow
// is denying every request.
//
// A caller's flush cycle must reconcile an exhausted window even when it has
// no pending delta to write. An exhausted window admits nothing, so it accrues
// no delta, so a flush cycle that skipped zero-delta windows would never call
// the backing store for it again — and the baseline is only ever refreshed by
// a flush. It would deny every request for the remaining life of the process,
// long after the window had rolled over in the store.
func (b *BatchingFixedWindow) Exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remoteTotal+b.pendingDelta >= b.limit
}

// ApplyRemoteTotal reconciles a successful flush. flushed is the delta value
// that was just synced to the backing store (the value PendingDelta returned
// before the flush began), and total is the authoritative count the backing
// store reported afterward. Subtracting exactly flushed — rather than
// resetting the pending delta to zero — keeps any admits that arrived while
// the flush was in flight correctly reflected against the new baseline.
func (b *BatchingFixedWindow) ApplyRemoteTotal(flushed int64, total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingDelta -= flushed
	b.remoteTotal = total
}
