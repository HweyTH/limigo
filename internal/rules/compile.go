package rules

import (
	"context"
	"fmt"

	"github.com/hweyth/limigo/internal/config"
	"github.com/hweyth/limigo/internal/store"
)

// Store is the union of backing-store capabilities a compiled rule set needs
// to evaluate every supported algorithm. RedisStore satisfies Store.
type Store interface {
	store.FixedWindowStore
	store.SlidingWindowStore
	store.TokenBucketStore
}

// CompiledRule pairs a rule's request matcher with a closure that evaluates
// the rule's configured algorithm against the backing Store.
type CompiledRule struct {
	// Name identifies the rule in logs and metrics.
	Name string
	// Match defines the request attributes that activate the rule.
	Match config.Match
	allow func(ctx context.Context, key string) (bool, error)
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
		return &CompiledRule{
			Name:  rule.Name,
			Match: rule.Match,
			allow: func(ctx context.Context, key string) (bool, error) {
				return st.AllowFixedWindow(ctx, key, limit, window)
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
				return st.AllowSlidingWindow(ctx, key, limit, window)
			},
		}, nil

	case config.TokenBucket:
		if rule.TokenBucket == nil {
			return nil, fmt.Errorf("token_bucket settings must be configured")
		}
		capacity, rate := rule.TokenBucket.Capacity, rule.TokenBucket.Rate
		return &CompiledRule{
			Name:  rule.Name,
			Match: rule.Match,
			allow: func(ctx context.Context, key string) (bool, error) {
				return st.AllowTokenBucket(ctx, key, capacity, rate)
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown algorithm %q", rule.Algorithm)
	}
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
