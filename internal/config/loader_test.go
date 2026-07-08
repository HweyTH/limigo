package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadParsesWindowDuration verifies YAML duration parsing for every supported algorithm.
func TestLoadParsesWindowDuration(t *testing.T) {
	tests := []struct {
		name      string
		algorithm Algorithm
	}{
		{name: "fixed window", algorithm: FixedWindow},
		{name: "sliding window", algorithm: SlidingWindow},
		{name: "token bucket", algorithm: TokenBucket},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yamlText := strings.Join([]string{
				"rules:",
				"  - name: free-tier",
				"    match:",
				"      header: X-Plan",
				"      value: free",
				"    limit: 100",
				"    window: 60s",
				fmt.Sprintf("    algorithm: %s", tc.algorithm),
				"",
			}, "\n")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")

			err := os.WriteFile(path, []byte(yamlText), 0644)
			if err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if len(cfg.Rules) != 1 {
				t.Fatalf("len(Rules) = %d, want 1", len(cfg.Rules))
			}
			if cfg.Rules[0].Window != time.Minute {
				t.Fatalf("Window = %v, want %v", cfg.Rules[0].Window, time.Minute)
			}
			if cfg.Rules[0].Algorithm != tc.algorithm {
				t.Fatalf("Algorithm = %v, want %v", cfg.Rules[0].Algorithm, tc.algorithm)
			}
		})
	}
}

// TestValidateAcceptsValidConfig verifies each supported algorithm passes validation.
func TestValidateAcceptsValidConfig(t *testing.T) {
	tests := []struct {
		name      string
		algorithm Algorithm
	}{
		{name: "fixed window", algorithm: FixedWindow},
		{name: "sliding window", algorithm: SlidingWindow},
		{name: "token bucket", algorithm: TokenBucket},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			cfg.Rules[0].Algorithm = tc.algorithm

			if err := Validate(cfg); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

// TestValidateRejectsBadConfig verifies validation fails with useful errors for invalid rules.
func TestValidateRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func() *Config
		wantErr string
	}{
		{
			name: "nil config",
			cfg: func() *Config {
				return nil
			},
			wantErr: "config must not be nil",
		},
		{
			name: "empty name",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Name = ""
				return cfg
			},
			wantErr: "missing a name",
		},
		{
			name: "whitespace name",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Name = "   "
				return cfg
			},
			wantErr: "missing a name",
		},
		{
			name: "empty match header",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Match.HeaderName = ""
				return cfg
			},
			wantErr: "match.header must not be empty",
		},
		{
			name: "whitespace match header",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Match.HeaderName = "   "
				return cfg
			},
			wantErr: "match.header must not be empty",
		},
		{
			name: "empty match value",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Match.Value = ""
				return cfg
			},
			wantErr: "match.value must not be empty",
		},
		{
			name: "whitespace match value",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Match.Value = "   "
				return cfg
			},
			wantErr: "match.value must not be empty",
		},
		{
			name: "zero limit",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Limit = 0
				return cfg
			},
			wantErr: "limit must be greater than zero",
		},
		{
			name: "negative limit",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Limit = -1
				return cfg
			},
			wantErr: "limit must be greater than zero",
		},
		{
			name: "zero window",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Window = 0
				return cfg
			},
			wantErr: "window must be greater than zero",
		},
		{
			name: "negative window",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Window = -time.Second
				return cfg
			},
			wantErr: "window must be greater than zero",
		},
		{
			name: "empty algorithm",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Algorithm = ""
				return cfg
			},
			wantErr: "algorithm must not be empty",
		},
		{
			name: "whitespace algorithm",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Algorithm = "   "
				return cfg
			},
			wantErr: "algorithm must not be empty",
		},
		{
			name: "unknown algorithm",
			cfg: func() *Config {
				cfg := validTestConfig()
				cfg.Rules[0].Algorithm = "leaky_bucket"
				return cfg
			},
			wantErr: "unknown algorithm",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg())
			if err == nil {
				t.Fatalf("Validate() error = nil, want containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoadRejectsInvalidWindowDuration verifies malformed duration strings fail during parsing.
func TestLoadRejectsInvalidWindowDuration(t *testing.T) {
	yamlText := strings.Join([]string{
		"rules:",
		"  - name: free-tier",
		"    match:",
		"      header: X-Plan",
		"      value: free",
		"    limit: 100",
		"    window: nope",
		"    algorithm: sliding_window",
		"",
	}, "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	err := os.WriteFile(path, []byte(yamlText), 0644)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
	if !strings.Contains(err.Error(), "failed to parse config file") {
		t.Fatalf("Load() error = %q, want parse error", err.Error())
	}
}

// validTestConfig returns the baseline config mutated by validation tests.
func validTestConfig() *Config {
	return &Config{
		Rules: []Rule{
			{
				Name: "free-tier",
				Match: Match{
					HeaderName: "X-Plan",
					Value:      "free",
				},
				Limit:     100,
				Window:    time.Minute,
				Algorithm: SlidingWindow,
			},
		},
	}
}
