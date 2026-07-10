package rules

import (
	"context"
	"sync/atomic"
)

// EngineHolder allows the active rule Engine to be swapped atomically while
// concurrent requests are being checked against it. The zero value is not
// usable; construct with NewEngineHolder.
type EngineHolder struct {
	engine atomic.Pointer[Engine]
}

// NewEngineHolder returns an EngineHolder initialized with engine.
func NewEngineHolder(engine *Engine) *EngineHolder {
	holder := &EngineHolder{}
	holder.Store(engine)
	return holder
}

// Store atomically replaces the active engine. Safe to call concurrently
// with Check.
func (holder *EngineHolder) Store(engine *Engine) {
	holder.engine.Store(engine)
}

// Check delegates to the currently active engine
func (holder *EngineHolder) Check(ctx context.Context, key string, headerValue func(string) string) (Decision, error) {
	return holder.engine.Load().Check(ctx, key, headerValue)
}

// FlushLocalCaches delegates to the currently active engine's FlushLocalCaches.
func (holder *EngineHolder) FlushLocalCaches(ctx context.Context) error {
	return holder.engine.Load().FlushLocalCaches(ctx)
}
