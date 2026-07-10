package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads the YAML config file at path and parses it into a Config.
// It does not validate the contents — call Validate after loading.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	return &cfg, nil
}

// Validate checks that all rules in cfg are well-formed.
// It returns an error for the first rule that fails, including the rule name
// in the message so the operator can identify and fix it quickly.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}

	for _, rule := range cfg.Rules {
		if err := validateRule(rule); err != nil {
			return err
		}
	}
	return nil
}

func validateRule(rule Rule) error {
	if strings.TrimSpace(rule.Name) == "" {
		return fmt.Errorf("a rule is missing a name")
	} else if strings.TrimSpace(rule.Match.HeaderName) == "" {
		return fmt.Errorf("rule %q: match.header must not be empty", rule.Name)
	} else if strings.TrimSpace(rule.Match.Value) == "" {
		return fmt.Errorf("rule %q: match.value must not be empty", rule.Name)
	} else if strings.TrimSpace(string(rule.Algorithm)) == "" {
		return fmt.Errorf("rule %q: algorithm must not be empty", rule.Name)
	}

	if err := validateLocalCache(rule); err != nil {
		return err
	}

	switch rule.Algorithm {
	case FixedWindow:
		return validateWindowLimit(rule, FixedWindow, rule.FixedWindow)
	case SlidingWindow:
		return validateWindowLimit(rule, SlidingWindow, rule.SlidingWindow)
	case TokenBucket:
		return validateTokenBucket(rule)
	default:
		return fmt.Errorf("rule %q: unknown algorithm %q", rule.Name, rule.Algorithm)
	}
}

func validateWindowLimit(rule Rule, algorithm Algorithm, params *WindowLimit) error {
	fieldName := string(algorithm)
	if params == nil {
		return fmt.Errorf("rule %q: %s settings must be configured", rule.Name, fieldName)
	} else if params.Limit <= 0 {
		return fmt.Errorf("rule %q: %s.limit must be greater than zero", rule.Name, fieldName)
	} else if params.Window <= 0 {
		return fmt.Errorf("rule %q: %s.window must be greater than zero", rule.Name, fieldName)
	}
	return validateNoExtraSettings(rule, algorithm)
}

func validateTokenBucket(rule Rule) error {
	if rule.TokenBucket == nil {
		return fmt.Errorf("rule %q: token_bucket settings must be configured", rule.Name)
	} else if rule.TokenBucket.Capacity <= 0 {
		return fmt.Errorf("rule %q: token_bucket.capacity must be greater than zero", rule.Name)
	} else if rule.TokenBucket.Rate <= 0 {
		return fmt.Errorf("rule %q: token_bucket.rate must be greater than zero", rule.Name)
	}
	return validateNoExtraSettings(rule, TokenBucket)
}

// validateLocalCache rejects local_cache on algorithms that cannot support
// batched delta reconciliation with an authoritative accuracy guarantee.
// Sliding window's exact rolling-timestamp accuracy is its main advantage
// over fixed window, so it deliberately does not support batching.
func validateLocalCache(rule Rule) error {
	if rule.LocalCache && rule.Algorithm == SlidingWindow {
		return fmt.Errorf("rule %q: local_cache is not supported for %s", rule.Name, SlidingWindow)
	}
	return nil
}

func validateNoExtraSettings(rule Rule, algorithm Algorithm) error {
	if algorithm != FixedWindow && rule.FixedWindow != nil {
		return fmt.Errorf("rule %q: fixed_window settings only apply when algorithm is %q", rule.Name, FixedWindow)
	}
	if algorithm != SlidingWindow && rule.SlidingWindow != nil {
		return fmt.Errorf("rule %q: sliding_window settings only apply when algorithm is %q", rule.Name, SlidingWindow)
	}
	if algorithm != TokenBucket && rule.TokenBucket != nil {
		return fmt.Errorf("rule %q: token_bucket settings only apply when algorithm is %q", rule.Name, TokenBucket)
	}
	return nil
}
