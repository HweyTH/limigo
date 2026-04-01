package config

import (
	"fmt"
	"os"

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
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	return &cfg, nil
}

// Validate checks that all rules in cfg are well-formed.
// It returns an error for the first rule that fails, including the rule name
// in the message so the operator can identify and fix it quickly.
func Validate(cfg *Config) error {
	for _, rule := range cfg.Rules {
		if rule.Name == "" {
			return fmt.Errorf("a rule is missing a name")
		} else if rule.Limit <= 0 {
			return fmt.Errorf("rule %q: limit must be greater than zero", rule.Name)
		} else if rule.Window == "" {
			return fmt.Errorf("rule %q: window must not be empty", rule.Name)
		} else if rule.Algorithm != TokenBucket && rule.Algorithm != SlidingWindow && rule.Algorithm != FixedWindow {
			return fmt.Errorf("rule %q: unknown algorithm %q", rule.Name, rule.Algorithm)
		}
	}
	return nil
}
