package rules

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hweyth/limigo/internal/config"
)

// fakeStore is an in-memory Store double that records which algorithm was
// invoked and lets tests script the returned decision.
type fakeStore struct {
	allow    bool
	err      error
	lastCall string
	lastKey  string
}

func (s *fakeStore) AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	s.lastCall = "fixed_window"
	s.lastKey = key
	return s.allow, s.err
}

func (s *fakeStore) AllowSlidingWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	s.lastCall = "sliding_window"
	s.lastKey = key
	return s.allow, s.err
}

func (s *fakeStore) AllowTokenBucket(ctx context.Context, key string, capacity float64, rate float64) (bool, error) {
	s.lastCall = "token_bucket"
	s.lastKey = key
	return s.allow, s.err
}

func TestCompile(t *testing.T) {
	t.Run("HappyPathCompilesOneRulePerAlgorithm", func(t *testing.T) {
		cfg := &config.Config{
			Rules: []config.Rule{
				{
					Name:      "fw-rule",
					Match:     config.Match{HeaderName: "X-Plan", Value: "fw"},
					Algorithm: config.FixedWindow,
					FixedWindow: &config.WindowLimit{
						Limit:  10,
						Window: time.Second,
					},
				},
				{
					Name:      "sw-rule",
					Match:     config.Match{HeaderName: "X-Plan", Value: "sw"},
					Algorithm: config.SlidingWindow,
					SlidingWindow: &config.WindowLimit{
						Limit:  10,
						Window: time.Second,
					},
				},
				{
					Name:      "tb-rule",
					Match:     config.Match{HeaderName: "X-Plan", Value: "tb"},
					Algorithm: config.TokenBucket,
					TokenBucket: &config.TokenBucketConfig{
						Capacity: 10,
						Rate:     1,
					},
				},
			},
		}

		engine, err := Compile(cfg, &fakeStore{allow: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(engine.compiledRules) != 3 {
			t.Fatalf("expected 3 compiled rules, got %d", len(engine.compiledRules))
		}
	})

	t.Run("UnhappyPathNilConfigReturnsError", func(t *testing.T) {
		if _, err := Compile(nil, &fakeStore{}); err == nil {
			t.Fatal("expected error for nil config")
		}
	})

	t.Run("UnhappyPathNilStoreReturnsError", func(t *testing.T) {
		cfg := &config.Config{}
		if _, err := Compile(cfg, nil); err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("UnhappyPathUnknownAlgorithmReturnsError", func(t *testing.T) {
		cfg := &config.Config{
			Rules: []config.Rule{
				{Name: "bad-rule", Algorithm: config.Algorithm("unknown")},
			},
		}
		if _, err := Compile(cfg, &fakeStore{}); err == nil {
			t.Fatal("expected error for unknown algorithm")
		}
	})

	t.Run("UnhappyPathMissingAlgorithmSettingsReturnsError", func(t *testing.T) {
		cfg := &config.Config{
			Rules: []config.Rule{
				{Name: "missing-settings", Algorithm: config.TokenBucket},
			},
		}
		if _, err := Compile(cfg, &fakeStore{}); err == nil {
			t.Fatal("expected error when token_bucket settings are missing")
		}
	})
}

func TestEngineEvaluate(t *testing.T) {
	newEngine := func(t *testing.T, store *fakeStore) *Engine {
		t.Helper()
		cfg := &config.Config{
			Rules: []config.Rule{
				{
					Name:      "free-tier",
					Match:     config.Match{HeaderName: "X-Plan", Value: "free"},
					Algorithm: config.FixedWindow,
					FixedWindow: &config.WindowLimit{
						Limit:  100,
						Window: time.Minute,
					},
				},
				{
					Name:      "pro-tier",
					Match:     config.Match{HeaderName: "X-Plan", Value: "pro"},
					Algorithm: config.TokenBucket,
					TokenBucket: &config.TokenBucketConfig{
						Capacity: 10000,
						Rate:     100,
					},
				},
			},
		}
		engine, err := Compile(cfg, store)
		if err != nil {
			t.Fatalf("unexpected compile error: %v", err)
		}
		return engine
	}

	t.Run("HappyPathMatchesRuleAndCallsCorrectAlgorithm", func(t *testing.T) {
		store := &fakeStore{allow: true}
		engine := newEngine(t, store)

		allowed, matched, err := engine.Evaluate(context.Background(), "X-Plan", "pro", "client-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !matched {
			t.Fatal("expected a rule to match")
		}
		if !allowed {
			t.Fatal("expected request to be allowed")
		}
		if store.lastCall != "token_bucket" {
			t.Fatalf("expected token_bucket to be invoked, got %q", store.lastCall)
		}
	})

	t.Run("UnhappyPathNoRuleMatches", func(t *testing.T) {
		store := &fakeStore{allow: true}
		engine := newEngine(t, store)

		allowed, matched, err := engine.Evaluate(context.Background(), "X-Plan", "enterprise", "client-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Fatal("expected no rule to match")
		}
		if allowed {
			t.Fatal("expected allowed to be false when no rule matches")
		}
		if store.lastCall != "" {
			t.Fatalf("expected store not to be called, got %q", store.lastCall)
		}
	})

	t.Run("UnhappyPathPropagatesStoreError", func(t *testing.T) {
		wantErr := errors.New("redis unavailable")
		store := &fakeStore{err: wantErr}
		engine := newEngine(t, store)

		_, matched, err := engine.Evaluate(context.Background(), "X-Plan", "free", "client-1")
		if !matched {
			t.Fatal("expected the free-tier rule to match")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected store error to propagate, got %v", err)
		}
	})
}

func TestEngineCheck(t *testing.T) {
	newEngine := func(t *testing.T, store *fakeStore) *Engine {
		t.Helper()
		cfg := &config.Config{
			Rules: []config.Rule{
				{
					Name:      "free-tier",
					Match:     config.Match{HeaderName: "X-Plan", Value: "free"},
					Algorithm: config.FixedWindow,
					FixedWindow: &config.WindowLimit{
						Limit:  100,
						Window: time.Minute,
					},
				},
				{
					Name:      "pro-tier",
					Match:     config.Match{HeaderName: "X-Plan", Value: "pro"},
					Algorithm: config.TokenBucket,
					TokenBucket: &config.TokenBucketConfig{
						Capacity: 10000,
						Rate:     100,
					},
				},
			},
		}
		engine, err := Compile(cfg, store)
		if err != nil {
			t.Fatalf("unexpected compile error: %v", err)
		}
		return engine
	}

	t.Run("HappyPathMatchesConfiguredHeaderAndAllowsRequest", func(t *testing.T) {
		store := &fakeStore{allow: true}
		engine := newEngine(t, store)

		decision, err := engine.Check(context.Background(), "client-1", func(name string) string {
			if name != "X-Plan" {
				t.Fatalf("header lookup name = %q, want X-Plan", name)
			}
			return "pro"
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !decision.Matched {
			t.Fatal("expected a rule to match")
		}
		if !decision.Allowed {
			t.Fatal("expected request to be allowed")
		}
		if decision.RuleName != "pro-tier" {
			t.Fatalf("RuleName = %q, want pro-tier", decision.RuleName)
		}
		if store.lastCall != "token_bucket" {
			t.Fatalf("expected token_bucket to be invoked, got %q", store.lastCall)
		}
		if store.lastKey != "limigo:pro-tier:client-1" {
			t.Fatalf("store key = %q, want limigo:pro-tier:client-1", store.lastKey)
		}
	})

	t.Run("UnhappyPathNoRuleMatches", func(t *testing.T) {
		store := &fakeStore{allow: true}
		engine := newEngine(t, store)

		decision, err := engine.Check(context.Background(), "client-1", func(string) string {
			return "enterprise"
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if decision.Matched {
			t.Fatal("expected no rule to match")
		}
		if decision.Allowed {
			t.Fatal("expected unmatched request to be denied")
		}
		if decision.RuleName != "" {
			t.Fatalf("RuleName = %q, want empty", decision.RuleName)
		}
		if store.lastCall != "" {
			t.Fatalf("expected store not to be called, got %q", store.lastCall)
		}
	})

	t.Run("UnhappyPathPropagatesStoreError", func(t *testing.T) {
		wantErr := errors.New("redis unavailable")
		store := &fakeStore{err: wantErr}
		engine := newEngine(t, store)

		decision, err := engine.Check(context.Background(), "client-1", func(string) string {
			return "free"
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected store error to propagate, got %v", err)
		}
		if !decision.Matched {
			t.Fatal("expected the free-tier rule to match")
		}
		if decision.Allowed {
			t.Fatal("expected errored request to be denied")
		}
		if decision.RuleName != "free-tier" {
			t.Fatalf("RuleName = %q, want free-tier", decision.RuleName)
		}
	})

	t.Run("UnhappyPathNilHeaderValueLookupReturnsError", func(t *testing.T) {
		store := &fakeStore{allow: true}
		engine := newEngine(t, store)

		_, err := engine.Check(context.Background(), "client-1", nil)
		if err == nil {
			t.Fatal("expected error for nil header lookup")
		}
		if store.lastCall != "" {
			t.Fatalf("expected store not to be called, got %q", store.lastCall)
		}
	})
}
