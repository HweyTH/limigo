package rules

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	store.LeakyBucketStore
	store.FixedWindowSyncStore
	store.TokenBucketSyncStore
}

// FillRatioRecorder receives the mean fill ratio of a rule's node-local token
// buckets, sampled on every flush. ClearTokenBucketFillRatio removes a rule's
// series entirely when it has no buckets, so a dashboard reads "no data"
// rather than a misleading flat zero.
type FillRatioRecorder interface {
	SetTokenBucketFillRatio(rule string, ratio float64)
	ClearTokenBucketFillRatio(rule string)
}

// CompiledRule pairs a rule's request matcher with a closure that evaluates
// the rule's configured algorithm against the backing Store.
type CompiledRule struct {
	// Name identifies the rule in logs and metrics.
	Name string
	// Match defines the request attributes that activate the rule.
	Match config.Match
	// limit and window are the rule's static quota, reported on every
	// Decision as the policy a caller is being held to: limit requests per
	// window for the window algorithms and leaky bucket; capacity with a
	// zero window for token bucket, which has no window to report.
	limit  int64
	window time.Duration
	allow  func(ctx context.Context, key string) (store.Verdict, error)
	// flush reconciles this rule's node-local cache with the backing Store.
	// It is nil unless the rule opted into local caching (config.Rule.LocalCache).
	flush func(ctx context.Context) error
}

// Matches reports whether headerName and headerValue activate this rule.
func (r *CompiledRule) Matches(headerName, headerValue string) bool {
	return headerName == r.Match.HeaderName && headerValue == r.Match.Value
}

// Allow evaluates the rule's configured algorithm for key. The Verdict
// carries the decision and the quota state behind it; see store.Verdict.
func (r *CompiledRule) Allow(ctx context.Context, key string) (store.Verdict, error) {
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
	// RetryAfter is the exact duration until the matched rule will next admit
	// a request, when the algorithm can compute it (currently only leaky
	// bucket) and the request was denied. It is zero otherwise.
	RetryAfter time.Duration
	// Limit is the matched rule's quota in requests: limit for the window
	// algorithms and leaky bucket, capacity for token bucket. Zero when
	// Matched is false.
	Limit int64
	// Window is the period Limit applies to. Zero for token bucket, which
	// refills continuously rather than per window, and when Matched is false.
	Window time.Duration
	// Remaining is the quota left after this decision. For a rule with
	// node-local caching it is this node's local view, not the fleet's: other
	// nodes' unflushed admits are invisible until the next flush, which is
	// the same bounded accuracy window the overshoot measurements quantify.
	Remaining int64
	// Reset is how long until the quota is fully restored (store.Verdict.Reset).
	// Zero when unknown — a locally cached rule does not track it — or when
	// Matched is false.
	Reset time.Duration
}

// Compile turns cfg's rules into an Engine backed by st. cfg is expected to
// have already passed config.Validate; Compile still checks that each rule's
// algorithm-specific settings are present so a bad rule fails loudly at
// startup rather than panicking at request time.
func Compile(cfg *config.Config, st Store, recorder FillRatioRecorder) (*Engine, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config must not be nil")
	}
	if st == nil {
		return nil, fmt.Errorf("store must not be nil")
	}

	compiledRules := make([]*CompiledRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		compiledRule, err := compileRule(rule, st, recorder)
		if err != nil {
			return nil, fmt.Errorf("compile rule %q: %w", rule.Name, err)
		}
		compiledRules = append(compiledRules, compiledRule)
	}
	return &Engine{compiledRules: compiledRules}, nil
}

// compileRule chooses the correct algorithm for rule and binds it to st,
// returning a CompiledRule ready for matching and evaluation.
func compileRule(rule config.Rule, st Store, recorder FillRatioRecorder) (*CompiledRule, error) {
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
			Name:   rule.Name,
			Match:  rule.Match,
			limit:  limit,
			window: window,
			allow: func(ctx context.Context, key string) (store.Verdict, error) {
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
			Name:   rule.Name,
			Match:  rule.Match,
			limit:  limit,
			window: window,
			allow: func(ctx context.Context, key string) (store.Verdict, error) {
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
			return compileBatchingTokenBucket(rule, st, capacity, rate, recorder), nil
		}
		return &CompiledRule{
			Name:  rule.Name,
			Match: rule.Match,
			limit: tokenBucketLimit(capacity),
			allow: func(ctx context.Context, key string) (store.Verdict, error) {
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				return st.AllowTokenBucket(ctx, storeKey, capacity, rate)
			},
		}, nil

	case config.LeakyBucket:
		if rule.LeakyBucket == nil {
			return nil, fmt.Errorf("leaky_bucket settings must be configured")
		}
		limit, window, burst := rule.LeakyBucket.Limit, rule.LeakyBucket.Window, rule.LeakyBucket.Burst
		return &CompiledRule{
			Name:   rule.Name,
			Match:  rule.Match,
			limit:  limit,
			window: window,
			allow: func(ctx context.Context, key string) (store.Verdict, error) {
				storeKey := fmt.Sprintf("limigo:%s:%s", rule.Name, key)
				return st.AllowLeakyBucket(ctx, storeKey, limit, window, burst)
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
		Name:   rule.Name,
		Match:  rule.Match,
		limit:  limit,
		window: window,
		// Remaining is this node's local view; Reset is left zero because the
		// local cache does not track when the window rolls over in the store.
		allow: func(ctx context.Context, key string) (store.Verdict, error) {
			allowed, remaining := manager.Admit(ctx, key)
			return store.Verdict{Allowed: allowed, Remaining: remaining}, nil
		},
		flush: func(ctx context.Context) error {
			var errs error
			for key, bw := range manager.Snapshot() {
				delta := bw.PendingDelta()
				// Exhausted windows are re-synced even with nothing to write;
				// skipping them wedges them permanently. See BatchingFixedWindow.Exhausted.
				if delta == 0 && !bw.Exhausted() {
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
func compileBatchingTokenBucket(rule config.Rule, st Store, capacity, rate float64, recorder FillRatioRecorder) *CompiledRule {
	manager := limiter.NewBatchingTokenBucketManager(capacity, rate)
	return &CompiledRule{
		Name:  rule.Name,
		Match: rule.Match,
		limit: tokenBucketLimit(capacity),
		// Remaining is this node's local view; Reset is left zero because the
		// local cache does not simulate refill between flushes.
		allow: func(ctx context.Context, key string) (store.Verdict, error) {
			allowed, remaining := manager.Admit(ctx, key)
			return store.Verdict{Allowed: allowed, Remaining: remaining}, nil
		},
		flush: func(ctx context.Context) error {
			var errs error
			snapshot := manager.Snapshot()
			var sum float64
			for key, tb := range snapshot {
				sum += tb.FillRatio()
				delta := tb.PendingDelta()
				// Exhausted buckets are re-synced even with nothing to write;
				// skipping them wedges them permanently. See BatchingTokenBucket.Exhausted.
				if delta == 0 && !tb.Exhausted() {
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
			if len(snapshot) > 0 {
				recorder.SetTokenBucketFillRatio(rule.Name, sum/float64(len(snapshot)))
			} else {
				recorder.ClearTokenBucketFillRatio(rule.Name)
			}
			return errs
		},
	}
}

// tokenBucketLimit is the quota a token bucket reports: its capacity, in
// whole tokens. Capacity is configured as a float so fractional refill rates
// have a matching type; a fractional capacity is floored here because the
// reported quota is a count of requests.
func tokenBucketLimit(capacity float64) int64 {
	if capacity < 0 {
		return 0
	}
	return int64(math.Floor(capacity))
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
	verdict, err := rule.Allow(ctx, key)
	return verdict.Allowed, true, err
}

// Check evaluates key against the first compiled rule whose configured header
// matches the value returned by headerValue. If no rule matches, Check makes no
// store call and returns Matched false; the caller decides the policy for
// unmatched requests.
func (e *Engine) Check(ctx context.Context, key string, headerValue func(string) string) (Decision, error) {
	if headerValue == nil {
		return Decision{}, fmt.Errorf("header value lookup must not be nil")
	}

	for _, rule := range e.compiledRules {
		if headerValue(rule.Match.HeaderName) == rule.Match.Value {
			verdict, err := rule.Allow(ctx, key)
			return Decision{
				Allowed:    verdict.Allowed,
				Matched:    true,
				RuleName:   rule.Name,
				RetryAfter: verdict.RetryAfter,
				Limit:      rule.limit,
				Window:     rule.window,
				Remaining:  verdict.Remaining,
				Reset:      verdict.Reset,
			}, err
		}
	}
	return Decision{Allowed: false, Matched: false}, nil
}
