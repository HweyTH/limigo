package store

import (
	"context"
	"testing"
	"time"
)

// TestVerdictQuota pins the quota fields every script returns beside the
// verdict — the numbers the RateLimit response headers are built from.
// Remaining counts down by one per admit and clamps at zero; Reset is a
// positive duration no longer than the algorithm's own horizon while the key
// holds state.
func TestVerdictQuota(t *testing.T) {
	store := newTestRedisStore()
	ctx := context.Background()

	tests := []struct {
		name string
		// allow runs one admission attempt against a fresh key chosen by the
		// test, returning the verdict.
		allow func(t *testing.T, key string) Verdict
		// quota is how many admits a fresh key grants before denying.
		quota int64
		// horizon bounds Reset from above.
		horizon time.Duration
	}{
		{
			name: "fixed window",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				v, err := store.AllowFixedWindow(ctx, key, 3, 5*time.Second)
				if err != nil {
					t.Fatalf("AllowFixedWindow: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 5 * time.Second,
		},
		{
			name: "sliding window",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				v, err := store.AllowSlidingWindow(ctx, key, 3, 5*time.Second)
				if err != nil {
					t.Fatalf("AllowSlidingWindow: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 5 * time.Second,
		},
		{
			name: "token bucket",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				// Refill is slow enough (1 token/s) that the three admits below
				// do not see a token come back mid-test.
				v, err := store.AllowTokenBucket(ctx, key, 3, 1)
				if err != nil {
					t.Fatalf("AllowTokenBucket: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 3 * time.Second, // capacity / rate: time from empty to full
		},
		{
			name: "leaky bucket",
			allow: func(t *testing.T, key string) Verdict {
				t.Helper()
				// 10 per 5s is a 500ms emission interval; burst 3 tolerates two
				// early arrivals, so a fresh key admits three in a row.
				v, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 3)
				if err != nil {
					t.Fatalf("AllowLeakyBucket: %v", err)
				}
				return v
			},
			quota:   3,
			horizon: 3 * 500 * time.Millisecond, // burst * emission interval: full schedule catch-up
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := redisTestKey(t, "quota")

			for i := int64(1); i <= tt.quota; i++ {
				v := tt.allow(t, key)
				if !v.Allowed {
					t.Fatalf("admit %d: allowed = false, want true (quota %d)", i, tt.quota)
				}
				if want := tt.quota - i; v.Remaining != want {
					t.Fatalf("admit %d: Remaining = %d, want %d", i, v.Remaining, want)
				}
				if v.Reset <= 0 || v.Reset > tt.horizon {
					t.Fatalf("admit %d: Reset = %s, want in (0, %s]", i, v.Reset, tt.horizon)
				}
			}

			v := tt.allow(t, key)
			if v.Allowed {
				t.Fatalf("admit %d: allowed = true, want denied past quota %d", tt.quota+1, tt.quota)
			}
			if v.Remaining != 0 {
				t.Fatalf("denied: Remaining = %d, want 0", v.Remaining)
			}
			if v.Reset <= 0 || v.Reset > tt.horizon {
				t.Fatalf("denied: Reset = %s, want in (0, %s]", v.Reset, tt.horizon)
			}
		})
	}

	t.Run("leaky bucket retry-after and reset agree on the schedule", func(t *testing.T) {
		key := redisTestKey(t, "gcra")
		first, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 1)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		if !first.Allowed || first.Remaining != 0 || first.RetryAfter != 0 {
			t.Fatalf("first = %+v, want allowed with no remaining burst and no retry-after", first)
		}
		second, err := store.AllowLeakyBucket(ctx, key, 10, 5*time.Second, 1)
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if second.Allowed {
			t.Fatal("second: allowed, want denied with no burst tolerance")
		}
		// With burst 1 there is no tolerance, so the next admit and the full
		// catch-up are the same moment: one emission interval away, minus
		// however long the two calls took.
		if second.RetryAfter <= 0 || second.RetryAfter > 500*time.Millisecond {
			t.Fatalf("second: RetryAfter = %s, want in (0, 500ms]", second.RetryAfter)
		}
		if second.Reset != second.RetryAfter {
			t.Fatalf("second: Reset = %s, RetryAfter = %s; with burst 1 they must agree", second.Reset, second.RetryAfter)
		}
	})
}
