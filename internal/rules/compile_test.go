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
	allow           bool
	err             error
	lastCall        string
	lastKey         string
	leakyRetryAfter time.Duration

	// syncErr, syncTotal, and syncRemaining let tests script the batched
	// delta-sync methods independently of the direct Allow* methods above.
	syncErr       error
	syncTotal     int64
	syncRemaining float64
	syncCalls     int
	lastSyncKey   string
	lastSyncDelta float64
}

// fakeFillRatioRecorder is a FillRatioRecorder double that records the last
// ratio set or clear per rule, for tests that don't care about fill ratio to
// pass without a real metrics implementation.
type fakeFillRatioRecorder struct {
	set     map[string]float64
	cleared map[string]bool
}

func newFakeFillRatioRecorder() *fakeFillRatioRecorder {
	return &fakeFillRatioRecorder{set: map[string]float64{}, cleared: map[string]bool{}}
}

func (r *fakeFillRatioRecorder) SetTokenBucketFillRatio(rule string, ratio float64) {
	r.set[rule] = ratio
	delete(r.cleared, rule)
}

func (r *fakeFillRatioRecorder) ClearTokenBucketFillRatio(rule string) {
	r.cleared[rule] = true
	delete(r.set, rule)
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

func (s *fakeStore) AllowLeakyBucket(ctx context.Context, key string, limit int64, window time.Duration, burst int64) (bool, time.Duration, error) {
	s.lastCall = "leaky_bucket"
	s.lastKey = key
	return s.allow, s.leakyRetryAfter, s.err
}

func (s *fakeStore) SyncFixedWindow(ctx context.Context, key string, delta int64, window time.Duration) (int64, error) {
	s.syncCalls++
	s.lastSyncKey = key
	s.lastSyncDelta = float64(delta)
	return s.syncTotal, s.syncErr
}

func (s *fakeStore) SyncTokenBucket(ctx context.Context, key string, delta float64, capacity float64, rate float64) (float64, error) {
	s.syncCalls++
	s.lastSyncKey = key
	s.lastSyncDelta = delta
	return s.syncRemaining, s.syncErr
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
				{
					Name:      "lb-rule",
					Match:     config.Match{HeaderName: "X-Plan", Value: "lb"},
					Algorithm: config.LeakyBucket,
					LeakyBucket: &config.LeakyBucketConfig{
						Limit:  10,
						Window: time.Second,
						Burst:  1,
					},
				},
			},
		}

		engine, err := Compile(cfg, &fakeStore{allow: true}, newFakeFillRatioRecorder())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(engine.compiledRules) != 4 {
			t.Fatalf("expected 4 compiled rules, got %d", len(engine.compiledRules))
		}
	})

	t.Run("UnhappyPathNilConfigReturnsError", func(t *testing.T) {
		if _, err := Compile(nil, &fakeStore{}, newFakeFillRatioRecorder()); err == nil {
			t.Fatal("expected error for nil config")
		}
	})

	t.Run("UnhappyPathNilStoreReturnsError", func(t *testing.T) {
		cfg := &config.Config{}
		if _, err := Compile(cfg, nil, newFakeFillRatioRecorder()); err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("UnhappyPathUnknownAlgorithmReturnsError", func(t *testing.T) {
		cfg := &config.Config{
			Rules: []config.Rule{
				{Name: "bad-rule", Algorithm: config.Algorithm("unknown")},
			},
		}
		if _, err := Compile(cfg, &fakeStore{}, newFakeFillRatioRecorder()); err == nil {
			t.Fatal("expected error for unknown algorithm")
		}
	})

	t.Run("UnhappyPathMissingAlgorithmSettingsReturnsError", func(t *testing.T) {
		cfg := &config.Config{
			Rules: []config.Rule{
				{Name: "missing-settings", Algorithm: config.TokenBucket},
			},
		}
		if _, err := Compile(cfg, &fakeStore{}, newFakeFillRatioRecorder()); err == nil {
			t.Fatal("expected error when token_bucket settings are missing")
		}
	})

	t.Run("UnhappyPathMissingLeakyBucketSettingsReturnsError", func(t *testing.T) {
		cfg := &config.Config{
			Rules: []config.Rule{
				{Name: "missing-settings", Algorithm: config.LeakyBucket},
			},
		}
		if _, err := Compile(cfg, &fakeStore{}, newFakeFillRatioRecorder()); err == nil {
			t.Fatal("expected error when leaky_bucket settings are missing")
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
		engine, err := Compile(cfg, store, newFakeFillRatioRecorder())
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
				{
					Name:      "steady-tier",
					Match:     config.Match{HeaderName: "X-Plan", Value: "steady"},
					Algorithm: config.LeakyBucket,
					LeakyBucket: &config.LeakyBucketConfig{
						Limit:  600,
						Window: time.Minute,
						Burst:  10,
					},
				},
			},
		}
		engine, err := Compile(cfg, store, newFakeFillRatioRecorder())
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

	t.Run("HappyPathRoutesLeakyBucketRule", func(t *testing.T) {
		store := &fakeStore{allow: true}
		engine := newEngine(t, store)

		decision, err := engine.Check(context.Background(), "client-1", func(name string) string {
			return "steady"
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
		if decision.RuleName != "steady-tier" {
			t.Fatalf("RuleName = %q, want steady-tier", decision.RuleName)
		}
		if store.lastCall != "leaky_bucket" {
			t.Fatalf("expected leaky_bucket to be invoked, got %q", store.lastCall)
		}
		if store.lastKey != "limigo:steady-tier:client-1" {
			t.Fatalf("store key = %q, want limigo:steady-tier:client-1", store.lastKey)
		}
	})

	t.Run("DeniedLeakyBucketPropagatesRetryAfter", func(t *testing.T) {
		store := &fakeStore{allow: false, leakyRetryAfter: 37 * time.Millisecond}
		engine := newEngine(t, store)

		decision, err := engine.Check(context.Background(), "client-1", func(name string) string {
			return "steady"
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if decision.Allowed {
			t.Fatal("expected request to be denied")
		}
		if decision.RetryAfter != 37*time.Millisecond {
			t.Fatalf("RetryAfter = %v, want 37ms", decision.RetryAfter)
		}
	})

	t.Run("DeniedTokenBucketDoesNotSetRetryAfter", func(t *testing.T) {
		store := &fakeStore{allow: false}
		engine := newEngine(t, store)

		decision, err := engine.Check(context.Background(), "client-1", func(name string) string {
			return "pro"
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if decision.Allowed {
			t.Fatal("expected request to be denied")
		}
		if decision.RetryAfter != 0 {
			t.Fatalf("RetryAfter = %v, want 0 for an algorithm that cannot compute it", decision.RetryAfter)
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

// TestCompileLocalCacheFixedWindow verifies that rules with LocalCache enabled
// decide Allow entirely locally (never calling the direct Allow* store
// methods), and that FlushLocalCaches is the only path that reconciles the
// local cache with the store and updates its baseline.
func TestCompileLocalCacheFixedWindow(t *testing.T) {
	newEngine := func(t *testing.T, store *fakeStore, limit int64) *Engine {
		t.Helper()
		cfg := &config.Config{
			Rules: []config.Rule{
				{
					Name:       "cached-tier",
					Match:      config.Match{HeaderName: "X-Plan", Value: "cached"},
					Algorithm:  config.FixedWindow,
					LocalCache: true,
					FixedWindow: &config.WindowLimit{
						Limit:  limit,
						Window: time.Minute,
					},
				},
			},
		}
		engine, err := Compile(cfg, store, newFakeFillRatioRecorder())
		if err != nil {
			t.Fatalf("unexpected compile error: %v", err)
		}
		return engine
	}

	check := func(t *testing.T, engine *Engine, key string) Decision {
		t.Helper()
		decision, err := engine.Check(context.Background(), key, func(name string) string {
			if name == "X-Plan" {
				return "cached"
			}
			return ""
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return decision
	}

	t.Run("AllowsLocallyWithoutCallingStore", func(t *testing.T) {
		store := &fakeStore{}
		engine := newEngine(t, store, 3)

		for i := range 3 {
			if decision := check(t, engine, "client-1"); !decision.Allowed {
				t.Fatalf("expected call %d to be allowed", i+1)
			}
		}

		if store.lastCall != "" {
			t.Fatalf("expected the direct AllowFixedWindow path never to be called, got %q", store.lastCall)
		}
		if store.syncCalls != 0 {
			t.Fatalf("expected no sync calls before a flush, got %d", store.syncCalls)
		}
	})

	t.Run("DeniesAfterLocalLimitExceeded", func(t *testing.T) {
		store := &fakeStore{}
		engine := newEngine(t, store, 2)

		check(t, engine, "client-1")
		check(t, engine, "client-1")
		if decision := check(t, engine, "client-1"); decision.Allowed {
			t.Fatal("expected request to be denied after local limit exceeded")
		}
	})

	t.Run("FlushReconcilesPendingDeltaAndUpdatesBaseline", func(t *testing.T) {
		store := &fakeStore{syncTotal: 2}
		engine := newEngine(t, store, 3)

		check(t, engine, "client-1")
		check(t, engine, "client-1")

		if err := engine.FlushLocalCaches(context.Background()); err != nil {
			t.Fatalf("unexpected flush error: %v", err)
		}

		if store.syncCalls != 1 {
			t.Fatalf("syncCalls = %d, want 1", store.syncCalls)
		}
		if store.lastSyncKey != "limigo:cached-tier:client-1" {
			t.Fatalf("lastSyncKey = %q, want limigo:cached-tier:client-1", store.lastSyncKey)
		}
		if store.lastSyncDelta != 2 {
			t.Fatalf("lastSyncDelta = %v, want 2", store.lastSyncDelta)
		}

		// Baseline is now remoteTotal=2 (from sync) + pendingDelta=0, so only
		// one more request should be allowed before hitting the limit of 3.
		if !check(t, engine, "client-1").Allowed {
			t.Fatal("expected one more request to be allowed after reconciling to baseline 2")
		}
		if check(t, engine, "client-1").Allowed {
			t.Fatal("expected request to be denied once reconciled baseline reaches the limit")
		}
	})

	t.Run("FlushIsNoOpWhenNothingPending", func(t *testing.T) {
		store := &fakeStore{}
		engine := newEngine(t, store, 3)

		if err := engine.FlushLocalCaches(context.Background()); err != nil {
			t.Fatalf("unexpected flush error: %v", err)
		}
		if store.syncCalls != 0 {
			t.Fatalf("syncCalls = %d, want 0 when no keys have pending admits", store.syncCalls)
		}
	})
}

// TestCompileLocalCacheTokenBucket verifies that token bucket rules with
// LocalCache enabled decide Allow entirely locally, and that FlushLocalCaches
// correctly reconciles a negative remaining-token count (fleet-wide overshoot).
func TestCompileLocalCacheTokenBucket(t *testing.T) {
	newEngine := func(t *testing.T, store *fakeStore, capacity float64) *Engine {
		t.Helper()
		cfg := &config.Config{
			Rules: []config.Rule{
				{
					Name:       "cached-burst",
					Match:      config.Match{HeaderName: "X-Plan", Value: "cached"},
					Algorithm:  config.TokenBucket,
					LocalCache: true,
					TokenBucket: &config.TokenBucketConfig{
						Capacity: capacity,
						Rate:     1,
					},
				},
			},
		}
		engine, err := Compile(cfg, store, newFakeFillRatioRecorder())
		if err != nil {
			t.Fatalf("unexpected compile error: %v", err)
		}
		return engine
	}

	check := func(t *testing.T, engine *Engine, key string) Decision {
		t.Helper()
		decision, err := engine.Check(context.Background(), key, func(name string) string {
			if name == "X-Plan" {
				return "cached"
			}
			return ""
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return decision
	}

	t.Run("AllowsLocallyWithoutCallingStore", func(t *testing.T) {
		store := &fakeStore{}
		engine := newEngine(t, store, 3)

		for i := range 3 {
			if decision := check(t, engine, "client-1"); !decision.Allowed {
				t.Fatalf("expected call %d to be allowed", i+1)
			}
		}

		if store.lastCall != "" {
			t.Fatalf("expected the direct AllowTokenBucket path never to be called, got %q", store.lastCall)
		}
		if store.syncCalls != 0 {
			t.Fatalf("expected no sync calls before a flush, got %d", store.syncCalls)
		}
	})

	t.Run("FlushReconcilesPendingDeltaAndCanGoNegative", func(t *testing.T) {
		store := &fakeStore{syncRemaining: -1}
		engine := newEngine(t, store, 3)

		check(t, engine, "client-1")
		check(t, engine, "client-1")
		check(t, engine, "client-1")

		if err := engine.FlushLocalCaches(context.Background()); err != nil {
			t.Fatalf("unexpected flush error: %v", err)
		}

		if store.lastSyncKey != "limigo:cached-burst:client-1" {
			t.Fatalf("lastSyncKey = %q, want limigo:cached-burst:client-1", store.lastSyncKey)
		}
		if store.lastSyncDelta != 3 {
			t.Fatalf("lastSyncDelta = %v, want 3", store.lastSyncDelta)
		}

		// Baseline is now remoteTokens=-1 (fleet-wide overshoot signal), so the
		// next request must be denied locally until a later flush restores headroom.
		if check(t, engine, "client-1").Allowed {
			t.Fatal("expected request to be denied while reconciled remaining tokens are negative")
		}
	})
}

// TestEngineFlushLocalCachesSkipsAndPropagatesErrors verifies that
// FlushLocalCaches skips rules without LocalCache entirely, and that a
// backing-store error on one LocalCache rule does not prevent another
// LocalCache rule from also being flushed.
func TestEngineFlushLocalCachesSkipsAndPropagatesErrors(t *testing.T) {
	cfg := &config.Config{
		Rules: []config.Rule{
			{
				Name:        "cached-fw",
				Match:       config.Match{HeaderName: "X-Plan", Value: "cached-fw"},
				Algorithm:   config.FixedWindow,
				LocalCache:  true,
				FixedWindow: &config.WindowLimit{Limit: 10, Window: time.Minute},
			},
			{
				Name:        "cached-tb",
				Match:       config.Match{HeaderName: "X-Plan", Value: "cached-tb"},
				Algorithm:   config.TokenBucket,
				LocalCache:  true,
				TokenBucket: &config.TokenBucketConfig{Capacity: 10, Rate: 1},
			},
			{
				Name:          "direct-sw",
				Match:         config.Match{HeaderName: "X-Plan", Value: "direct-sw"},
				Algorithm:     config.SlidingWindow,
				SlidingWindow: &config.WindowLimit{Limit: 10, Window: time.Minute},
			},
		},
	}

	wantErr := errors.New("redis unavailable")
	store := &fakeStore{syncErr: wantErr}
	engine, err := Compile(cfg, store, newFakeFillRatioRecorder())
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	ctx := context.Background()
	doCheck := func(headerValue string) {
		if _, err := engine.Check(ctx, "client-1", func(name string) string {
			if name == "X-Plan" {
				return headerValue
			}
			return ""
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	doCheck("cached-fw")
	doCheck("cached-tb")
	doCheck("direct-sw") // not local_cache; must never be flushed

	err = engine.FlushLocalCaches(ctx)
	if err == nil {
		t.Fatal("expected FlushLocalCaches to return a joined error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected joined error to wrap the store error, got %v", err)
	}
	if store.syncCalls != 2 {
		t.Fatalf("syncCalls = %d, want 2 (both LocalCache rules attempted despite one failing)", store.syncCalls)
	}
}
