// Package config handles loading and validating the Limigo YAML configuration file.
// Use Load to parse a config file, then Validate to check its contents before use.
package config

type Algorithm string

const (
	TokenBucket   Algorithm = "token_bucket"
	SlidingWindow Algorithm = "sliding_window"
	FixedWindow   Algorithm = "fixed_window"
)

type Match struct {
	HeaderName string `yaml:"header"`
	Value      string `yaml:"value"`
}

type Config struct {
	Rules []Rule `yaml:"rules"`
}

type Rule struct {
	Name      string    `yaml:"name"`
	Match     Match     `yaml:"match"`
	Limit     int64     `yaml:"limit"`
	Window    string    `yaml:"window"`
	Algorithm Algorithm `yaml:"algorithm"`
}
