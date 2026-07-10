package rules

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hweyth/limigo/internal/config"
	"github.com/hweyth/limigo/internal/limiter"
	"github.com/hweyth/limigo/internal/store"
)

// Store is the union of backing-store capabilities a compiled rule set needs
// to evaluate every supported algorithm, including the batched delta-sync
// variants used by rules with node-local caching enabled. RedisStore
// satisfies Store.
type Store interface {
	store.FixedWindowStore
	store.SlidingWindowStore
	store.TokenBucketStore
	store.FixedWindowSyncStore
	store.TokenBucketSyncStore
}

// CompiledRule pairs a rule's request matcher with a closure that evaluates
// the rule's configured algorithm against the backing Store.
type CompiledRule struct {
	// Name identifies the rule in logs and metrics.
	Name string
	// Match defines the request attributes that activate the rule.
	Match config.Match
	allow func(ctx context.Context, key string) (bool, error)
	// flush reconciles this rule's node-local cache with the backing Store.
	// It is nil unless the rule opted into local caching (config.Rule.LocalCache).
	flush func(ctx context.Context) error
}

// Matches reports whether headerName and headerValue activate this rule.
func (r *CompiledRule) Matches(headerName, headerValue string) bool {
	return headerName == r.Match.HeaderName && headerValue == r.Match.Value
}

// Allow evaluates the rule's configured algorithm for key, returning true if
// the request is within limit, false if it should be throttled.
func (r *CompiledRule) Allow(ctx context.Context, key string) (bool, error) {
	return r.allow(ctx, key)
}

// Engine holds a compiled, ordered set of rules ready for request-time
// evaluation. It is safe for concurrent use — CompiledRules are immutable
// after Compile returns.
type Engine struct {
	compiledRules []*CompiledRule
}

// Decision describes the outcome of checking a request key against the first
// configured rule matched by request headers.
type Decision struct {
	// Allowed reports whether the request is within the matched rule's limit.
	Allowed bool
	// Matched reports whether any configured rule matched the request headers.
	Matched bool
	// RuleName identifies the matched rule. It is empty when Matched is false.
	RuleName string
}

// Compile turns cfg's rules into an Engine backed by st. cfg is expected to
// have already passed config.Validate; Compile still checks that each rule's
// algorithm-specific settings are present so a bad rule fails loudly at
// startup rather than panicking at request time.
func Compile(cfg *config.Config, st Store) (*Engine, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config must not be nil")
	}
	if st == nil {
		return nil, fmt.Errorf("store must not be nil")
	}

	compiledRules := make([]*CompiledRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		compiledRule, err := compileRule(rule, st)
		if err != nil {
			return nil, fmt.Errorf("compile rule %q: %w", rule.Name, err)
		}
		compiledRules = append(compiledRules, compiledRule)
	}
	return &Engine{compiledRules: compiledRules}, nil
}

// compileRule chooses the correct algorithm for rule and binds it to st,
// returning a CompiledRule ready for matching and evaluation.
func compileRule(rule config.Rule, st Store) (*CompiledRule, error) {
	switch rule.Algorithm {
	case config.FixedWindow:
		if rule.FixedWindow == nil {
			return nil, fmt.Errorf("fixed_window settings must be configured")
		}
		limit, window := rule.FixedWindow.Limit, rule.FixedWindow.Window
		if rule.LocalCache {
			return compileBatchingFixedWindow(rule, st, limit, window), nil
		}
		return &CompiledRule{
			Name:  rule.Name,
			Match: rule.Match,
			allow: func(ctx context.Context, key string) (bool, error) {
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				return st.AllowFixedWindow(ctx, storeKey, limit, window)
			},
		}, nil

	case config.SlidingWindow:
		if rule.SlidingWindow == nil {
			return nil, fmt.Errorf("sliding_window settings must be configured")
		}
		limit, window := rule.SlidingWindow.Limit, rule.SlidingWindow.Window
		return &CompiledRule{
			Name:  rule.Name,
			Match: rule.Match,
			allow: func(ctx context.Context, key string) (bool, error) {
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				return st.AllowSlidingWindow(ctx, storeKey, limit, window)
			},
		}, nil

	case config.TokenBucket:
		if rule.TokenBucket == nil {
			return nil, fmt.Errorf("token_bucket settings must be configured")
		}
		capacity, rate := rule.TokenBucket.Capacity, rule.TokenBucket.Rate
		if rule.LocalCache {
			return compileBatchingTokenBucket(rule, st, capacity, rate), nil
		}
		return &CompiledRule{
			Name:  rule.Name,
			Match: rule.Match,
			allow: func(ctx context.Context, key string) (bool, error) {
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				return st.AllowTokenBucket(ctx, storeKey, capacity, rate)
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown algorithm %q", rule.Algorithm)
	}
}

// compileBatchingFixedWindow builds a CompiledRule that decides admits
// against a node-local cache (see limiter.BatchingFixedWindowManager) instead
// of calling st on every request, trading a small accuracy window for lower
// request latency and reduced load on st. flush reconciles the local cache
// with st and must be driven periodically by the caller (see Engine.FlushLocalCaches).
func compileBatchingFixedWindow(rule config.Rule, st Store, limit int64, window time.Duration) *CompiledRule {
	manager := limiter.NewBatchingFixedWindowManager(limit)
	return &CompiledRule{
		Name:  rule.Name,
		Match: rule.Match,
		allow: func(ctx context.Context, key string) (bool, error) {
			return manager.Allow(ctx, key)
		},
		flush: func(ctx context.Context) error {
			var errs error
			for key, bw := range manager.Snapshot() {
				delta := bw.PendingDelta()
				if delta == 0 {
					continue
				}
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				total, err := st.SyncFixedWindow(ctx, storeKey, delta, window)
				if err != nil {
					errs = errors.Join(errs, fmt.Errorf("sync fixed window %q: %w", storeKey, err))
					continue
				}
				bw.ApplyRemoteTotal(delta, total)
			}
			return errs
		},
	}
}

// compileBatchingTokenBucket builds a CompiledRule that decides admits
// against a node-local cache (see limiter.BatchingTokenBucketManager) instead
// of calling st on every request, trading a small accuracy window for lower
// request latency and reduced load on st. flush reconciles the local cache
// with st and must be driven periodically by the caller (see Engine.FlushLocalCaches).
func compileBatchingTokenBucket(rule config.Rule, st Store, capacity, rate float64) *CompiledRule {
	manager := limiter.NewBatchingTokenBucketManager(capacity, rate)
	return &CompiledRule{
		Name:  rule.Name,
		Match: rule.Match,
		allow: func(ctx context.Context, key string) (bool, error) {
			return manager.Allow(ctx, key)
		},
		flush: func(ctx context.Context) error {
			var errs error
			for key, tb := range manager.Snapshot() {
				delta := tb.PendingDelta()
				if delta == 0 {
					continue
				}
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				remaining, err := st.SyncTokenBucket(ctx, storeKey, delta, capacity, rate)
				if err != nil {
					errs = errors.Join(errs, fmt.Errorf("sync token bucket %q: %w", storeKey, err))
					continue
				}
				tb.ApplyRemoteTotal(delta, remaining)
			}
			return errs
		},
	}
}

// FlushLocalCaches reconciles every rule using node-local caching (see
// config.Rule.LocalCache) with the backing store. Rules without local caching
// enabled are skipped. Errors from individual rules are joined so one rule's
// backing-store failure does not prevent others from flushing.
func (e *Engine) FlushLocalCaches(ctx context.Context) error {
	var errs error
	for _, rule := range e.compiledRules {
		if rule.flush == nil {
			continue
		}
		if err := rule.flush(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("rule %q: %w", rule.Name, err))
		}
	}
	return errs
}

// Match returns the first compiled rule whose matcher is activated by
// headerName and headerValue. ok is false if no rule matches.
func (e *Engine) Match(headerName, headerValue string) (rule *CompiledRule, ok bool) {
	for _, compiledRule := range e.compiledRules {
		if compiledRule.Matches(headerName, headerValue) {
			return compiledRule, true
		}
	}
	return nil, false
}

// Evaluate matches headerName/headerValue against the compiled rules and, if
// a rule matches, evaluates it for key. matched reports whether a rule
// matched; when matched is false, allowed and err are meaningless and the
// caller is responsible for deciding the default policy for unmatched
// requests.
func (e *Engine) Evaluate(ctx context.Context, headerName, headerValue, key string) (allowed bool, matched bool, err error) {
	rule, ok := e.Match(headerName, headerValue)
	if !ok {
		return false, false, nil
	}
	allowed, err = rule.Allow(ctx, key)
	return allowed, true, err
}

// Check evaluates key against the first compiled rule whose configured header
// matches the value returned by headerValue. If no rule matches, Check returns
// a fail-closed decision with Matched false and no store call.
func (e *Engine) Check(ctx context.Context, key string, headerValue func(string) string) (Decision, error) {
	if headerValue == nil {
		return Decision{}, fmt.Errorf("header value lookup must not be nil")
	}

	for _, rule := range e.compiledRules {
		if headerValue(rule.Match.HeaderName) == rule.Match.Value {
			allowed, err := rule.Allow(ctx, key)
			return Decision{
				Allowed:  allowed,
				Matched:  true,
				RuleName: rule.Name,
			}, err
		}
	}
	return Decision{Allowed: false, Matched: false}, nil
}
