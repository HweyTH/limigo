package config

import "time"

// Algorithm identifies the rate limiting algorithm selected by a rule.
type Algorithm string

const (
	// TokenBucket selects the token bucket algorithm.
	TokenBucket Algorithm = "token_bucket"
	// SlidingWindow selects the sliding window log algorithm.
	SlidingWindow Algorithm = "sliding_window"
	// FixedWindow selects the fixed window counter algorithm.
	FixedWindow Algorithm = "fixed_window"
)

// Match describes how an incoming request is matched to a rule.
type Match struct {
	// HeaderName is the request header used to select a matching rule.
	HeaderName string `yaml:"header"`
	// Value is the header value that activates the rule.
	Value string `yaml:"value"`
}

// Config is the root Limigo configuration document.
type Config struct {
	// Rules contains the ordered rate limiting rules loaded from configuration.
	Rules []Rule `yaml:"rules"`
}

// WindowLimit defines the limit and duration used by window-based algorithms.
type WindowLimit struct {
	// Limit is the maximum number of requests allowed within Window.
	Limit int64 `yaml:"limit"`
	// Window is the configured rate limit duration, such as 60s.
	Window time.Duration `yaml:"window"`
}

// TokenBucketConfig defines the capacity and refill rate used by token bucket rules.
type TokenBucketConfig struct {
	// Capacity is the maximum number of tokens the bucket can hold.
	Capacity float64 `yaml:"capacity"`
	// Rate is the number of tokens added to the bucket per second.
	Rate float64 `yaml:"rate"`
}

// Rule defines a named rate limit, its matcher, and algorithm-specific settings.
type Rule struct {
	// Name identifies the rule in logs, metrics, and validation errors.
	Name string `yaml:"name"`
	// Match defines the request attributes that activate the rule.
	Match Match `yaml:"match"`
	// Algorithm selects the limiter implementation for the rule.
	Algorithm Algorithm `yaml:"algorithm"`
	// FixedWindow defines parameters for the fixed window counter algorithm.
	FixedWindow *WindowLimit `yaml:"fixed_window,omitempty"`
	// SlidingWindow defines parameters for the sliding window log algorithm.
	SlidingWindow *WindowLimit `yaml:"sliding_window,omitempty"`
	// TokenBucket defines parameters for the token bucket algorithm.
	TokenBucket *TokenBucketConfig `yaml:"token_bucket,omitempty"`
	// LocalCache opts this rule into node-local burst absorption: requests are
	// admitted against an in-process cache and periodically reconciled with
	// Redis, trading a small accuracy window for lower latency and Redis load.
	// Only supported for Fixed Window and Token Bucket
	LocalCache bool `yaml:"local_cache,omitempty"`
}
