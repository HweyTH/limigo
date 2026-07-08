package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadParsesAlgorithmSettings verifies YAML parsing for each supported rule shape.
func TestLoadParsesAlgorithmSettings(t *testing.T) {
	tests := []struct {
		name      string
		yamlText  string
		assertion func(t *testing.T, rule Rule)
	}{
		{
			name: "fixed window",
			yamlText: strings.Join([]string{
				"rules:",
				"  - name: free-tier",
				"    match:",
				"      header: X-Plan",
				"      value: free",
				"    algorithm: fixed_window",
				"    fixed_window:",
				"      limit: 100",
				"      window: 60s",
				"",
			}, "\n"),
			assertion: func(t *testing.T, rule Rule) {
				t.Helper()
				if rule.FixedWindow == nil {
					t.Fatal("FixedWindow = nil, want settings")
				}
				if rule.FixedWindow.Limit != 100 {
					t.Fatalf("FixedWindow.Limit = %d, want 100", rule.FixedWindow.Limit)
				}
				if rule.FixedWindow.Window != time.Minute {
					t.Fatalf("FixedWindow.Window = %v, want %v", rule.FixedWindow.Window, time.Minute)
				}
			},
		},
		{
			name: "sliding window",
			yamlText: strings.Join([]string{
				"rules:",
				"  - name: pro-tier",
				"    match:",
				"      header: X-Plan",
				"      value: pro",
				"    algorithm: sliding_window",
				"    sliding_window:",
				"      limit: 10000",
				"      window: 60s",
				"",
			}, "\n"),
			assertion: func(t *testing.T, rule Rule) {
				t.Helper()
				if rule.SlidingWindow == nil {
					t.Fatal("SlidingWindow = nil, want settings")
				}
				if rule.SlidingWindow.Limit != 10000 {
					t.Fatalf("SlidingWindow.Limit = %d, want 10000", rule.SlidingWindow.Limit)
				}
				if rule.SlidingWindow.Window != time.Minute {
					t.Fatalf("SlidingWindow.Window = %v, want %v", rule.SlidingWindow.Window, time.Minute)
				}
			},
		},
		{
			name: "token bucket",
			yamlText: strings.Join([]string{
				"rules:",
				"  - name: burst-tier",
				"    match:",
				"      header: X-Plan",
				"      value: burst",
				"    algorithm: token_bucket",
				"    token_bucket:",
				"      capacity: 1000",
				"      rate: 200",
				"",
			}, "\n"),
			assertion: func(t *testing.T, rule Rule) {
				t.Helper()
				if rule.TokenBucket == nil {
					t.Fatal("TokenBucket = nil, want settings")
				}
				if rule.TokenBucket.Capacity != 1000 {
					t.Fatalf("TokenBucket.Capacity = %v, want 1000", rule.TokenBucket.Capacity)
				}
				if rule.TokenBucket.Rate != 200 {
					t.Fatalf("TokenBucket.Rate = %v, want 200", rule.TokenBucket.Rate)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfigFromYAML(t, tc.yamlText)
			if len(cfg.Rules) != 1 {
				t.Fatalf("len(Rules) = %d, want 1", len(cfg.Rules))
			}
			if cfg.Rules[0].Algorithm == "" {
				t.Fatal("Algorithm is empty")
			}
			tc.assertion(t, cfg.Rules[0])
		})
	}
}

// TestValidateAcceptsValidConfig verifies each supported algorithm passes validation.
func TestValidateAcceptsValidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{name: "fixed window", cfg: validWindowConfig(FixedWindow)},
		{name: "sliding window", cfg: validWindowConfig(SlidingWindow)},
		{name: "token bucket", cfg: validTokenBucketConfig()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.cfg); err != nil {
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
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Name = ""
				return cfg
			},
			wantErr: "missing a name",
		},
		{
			name: "whitespace name",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Name = "   "
				return cfg
			},
			wantErr: "missing a name",
		},
		{
			name: "empty match header",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Match.HeaderName = ""
				return cfg
			},
			wantErr: "match.header must not be empty",
		},
		{
			name: "whitespace match header",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Match.HeaderName = "   "
				return cfg
			},
			wantErr: "match.header must not be empty",
		},
		{
			name: "empty match value",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Match.Value = ""
				return cfg
			},
			wantErr: "match.value must not be empty",
		},
		{
			name: "whitespace match value",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Match.Value = "   "
				return cfg
			},
			wantErr: "match.value must not be empty",
		},
		{
			name: "empty algorithm",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Algorithm = ""
				return cfg
			},
			wantErr: "algorithm must not be empty",
		},
		{
			name: "whitespace algorithm",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Algorithm = "   "
				return cfg
			},
			wantErr: "algorithm must not be empty",
		},
		{
			name: "unknown algorithm",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].Algorithm = "leaky_bucket"
				return cfg
			},
			wantErr: "unknown algorithm",
		},
		{
			name: "missing fixed window settings",
			cfg: func() *Config {
				cfg := validWindowConfig(FixedWindow)
				cfg.Rules[0].FixedWindow = nil
				return cfg
			},
			wantErr: "fixed_window settings must be configured",
		},
		{
			name: "zero fixed window limit",
			cfg: func() *Config {
				cfg := validWindowConfig(FixedWindow)
				cfg.Rules[0].FixedWindow.Limit = 0
				return cfg
			},
			wantErr: "fixed_window.limit must be greater than zero",
		},
		{
			name: "negative fixed window limit",
			cfg: func() *Config {
				cfg := validWindowConfig(FixedWindow)
				cfg.Rules[0].FixedWindow.Limit = -1
				return cfg
			},
			wantErr: "fixed_window.limit must be greater than zero",
		},
		{
			name: "zero sliding window",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].SlidingWindow.Window = 0
				return cfg
			},
			wantErr: "sliding_window.window must be greater than zero",
		},
		{
			name: "negative sliding window",
			cfg: func() *Config {
				cfg := validWindowConfig(SlidingWindow)
				cfg.Rules[0].SlidingWindow.Window = -time.Second
				return cfg
			},
			wantErr: "sliding_window.window must be greater than zero",
		},
		{
			name: "missing token bucket settings",
			cfg: func() *Config {
				cfg := validTokenBucketConfig()
				cfg.Rules[0].TokenBucket = nil
				return cfg
			},
			wantErr: "token_bucket settings must be configured",
		},
		{
			name: "zero token bucket capacity",
			cfg: func() *Config {
				cfg := validTokenBucketConfig()
				cfg.Rules[0].TokenBucket.Capacity = 0
				return cfg
			},
			wantErr: "token_bucket.capacity must be greater than zero",
		},
		{
			name: "negative token bucket capacity",
			cfg: func() *Config {
				cfg := validTokenBucketConfig()
				cfg.Rules[0].TokenBucket.Capacity = -1
				return cfg
			},
			wantErr: "token_bucket.capacity must be greater than zero",
		},
		{
			name: "zero token bucket rate",
			cfg: func() *Config {
				cfg := validTokenBucketConfig()
				cfg.Rules[0].TokenBucket.Rate = 0
				return cfg
			},
			wantErr: "token_bucket.rate must be greater than zero",
		},
		{
			name: "negative token bucket rate",
			cfg: func() *Config {
				cfg := validTokenBucketConfig()
				cfg.Rules[0].TokenBucket.Rate = -1
				return cfg
			},
			wantErr: "token_bucket.rate must be greater than zero",
		},
		{
			name: "fixed window rejects token bucket settings",
			cfg: func() *Config {
				cfg := validWindowConfig(FixedWindow)
				cfg.Rules[0].TokenBucket = &TokenBucketConfig{Capacity: 100, Rate: 10}
				return cfg
			},
			wantErr: "token_bucket settings only apply when algorithm is \"token_bucket\"",
		},
		{
			name: "token bucket rejects sliding window settings",
			cfg: func() *Config {
				cfg := validTokenBucketConfig()
				cfg.Rules[0].SlidingWindow = &WindowLimit{Limit: 100, Window: time.Minute}
				return cfg
			},
			wantErr: "sliding_window settings only apply when algorithm is \"sliding_window\"",
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
		"    algorithm: sliding_window",
		"    sliding_window:",
		"      limit: 100",
		"      window: nope",
		"",
	}, "\n")

	_, err := loadConfigFromYAMLError(t, yamlText)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
	if !strings.Contains(err.Error(), "failed to parse config file") {
		t.Fatalf("Load() error = %q, want parse error", err.Error())
	}
}

// TestLoadRejectsUnknownFields verifies stale or misspelled YAML fields are not ignored.
func TestLoadRejectsUnknownFields(t *testing.T) {
	yamlText := strings.Join([]string{
		"rules:",
		"  - name: burst-tier",
		"    match:",
		"      header: X-Plan",
		"      value: burst",
		"    limit: 1000",
		"    window: 60s",
		"    algorithm: token_bucket",
		"    token_bucket:",
		"      capacity: 1000",
		"      rate: 200",
		"",
	}, "\n")

	_, err := loadConfigFromYAMLError(t, yamlText)
	if err == nil {
		t.Fatal("Load() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), "field limit not found") {
		t.Fatalf("Load() error = %q, want unknown limit field error", err.Error())
	}
}

func loadConfigFromYAML(t *testing.T, yamlText string) *Config {
	t.Helper()
	cfg, err := loadConfigFromYAMLError(t, yamlText)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return cfg
}

func loadConfigFromYAMLError(t *testing.T, yamlText string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(yamlText), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return Load(path)
}

func validWindowConfig(algorithm Algorithm) *Config {
	rule := validBaseRule(algorithm)
	params := &WindowLimit{Limit: 100, Window: time.Minute}
	if algorithm == FixedWindow {
		rule.FixedWindow = params
	} else {
		rule.SlidingWindow = params
	}
	return &Config{Rules: []Rule{rule}}
}

func validTokenBucketConfig() *Config {
	rule := validBaseRule(TokenBucket)
	rule.TokenBucket = &TokenBucketConfig{Capacity: 1000, Rate: 200}
	return &Config{Rules: []Rule{rule}}
}

func validBaseRule(algorithm Algorithm) Rule {
	return Rule{
		Name: "free-tier",
		Match: Match{
			HeaderName: "X-Plan",
			Value:      "free",
		},
		Algorithm: algorithm,
	}
}
