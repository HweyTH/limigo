package config

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

// Rule defines a named rate limit and the request matcher that activates it.
type Rule struct {
	// Name identifies the rule in logs, metrics, and validation errors.
	Name string `yaml:"name"`
	// Match defines the request attributes that activate the rule.
	Match Match `yaml:"match"`
	// Limit is the maximum number of requests allowed within Window.
	Limit int64 `yaml:"limit"`
	// Window is the configured rate limit duration, such as 60s.
	Window string `yaml:"window"`
	// Algorithm selects the limiter implementation for the rule.
	Algorithm Algorithm `yaml:"algorithm"`
}
