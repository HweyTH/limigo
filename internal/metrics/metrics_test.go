package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m.RecordRequest("free-tier", "allowed")
	m.RecordRequest("free-tier", "allowed")
	m.RecordRequest("free-tier", "denied")
	m.RecordRequest("pro-tier", "error")

	cases := []struct {
		result, rule string
		want         float64
	}{
		{"allowed", "free-tier", 2},
		{"denied", "free-tier", 1},
		{"error", "pro-tier", 1},
	}
	for _, c := range cases {
		if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues(c.result, c.rule)); got != c.want {
			t.Errorf("requests_total{result=%q,rule=%q} = %v, want %v", c.result, c.rule, got, c.want)
		}
	}
}

func TestIncConfigReload(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m.IncConfigReload(true)
	m.IncConfigReload(false)
	m.IncConfigReload(false)

	if got := testutil.ToFloat64(m.configReloadTotal.WithLabelValues("success")); got != 1 {
		t.Errorf("config_reload_total{result=success} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.configReloadTotal.WithLabelValues("failure")); got != 2 {
		t.Errorf("config_reload_total{result=failure} = %v, want 2", got)
	}
}

func TestTokenBucketFillRatioSetAndClear(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m.SetTokenBucketFillRatio("burst-tier", 0.75)
	if got := testutil.ToFloat64(m.tokenBucketFillRatio.WithLabelValues("burst-tier")); got != 0.75 {
		t.Errorf("token_bucket_fill_ratio{rule=burst-tier} = %v, want 0.75", got)
	}

	m.ClearTokenBucketFillRatio("burst-tier")
	if n := testutil.CollectAndCount(m.tokenBucketFillRatio); n != 0 {
		t.Errorf("token_bucket_fill_ratio series count after Clear = %d, want 0 (deleted series reads as no data, not zero)", n)
	}
}

func TestObserveRedisLatency(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m.ObserveRedisLatency("token_bucket", 3*time.Millisecond)

	expected := strings.NewReader(`
		# HELP limigo_redis_latency_seconds Round-trip latency of Redis calls, by algorithm.
		# TYPE limigo_redis_latency_seconds histogram
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="5e-05"} 0
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.0001"} 0
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.00025"} 0
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.0005"} 0
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.001"} 0
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.0025"} 0
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.005"} 1
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.01"} 1
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.025"} 1
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.05"} 1
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.1"} 1
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.25"} 1
		limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="+Inf"} 1
		limigo_redis_latency_seconds_sum{algorithm="token_bucket"} 0.003
		limigo_redis_latency_seconds_count{algorithm="token_bucket"} 1
	`)
	if err := testutil.CollectAndCompare(m.redisLatency, expected, "limigo_redis_latency_seconds"); err != nil {
		t.Errorf("unexpected collecting result:\n%s", err)
	}
}

func TestObserveLuaExecution(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m.ObserveLuaExecution("fixed_window", 40*time.Microsecond)

	expected := strings.NewReader(`
		# HELP limigo_lua_execution_seconds Server-side execution time of Lua scripts, by algorithm.
		# TYPE limigo_lua_execution_seconds histogram
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="5e-05"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.0001"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.00025"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.0005"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.001"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.0025"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.005"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.01"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.025"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.05"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.1"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="0.25"} 1
		limigo_lua_execution_seconds_bucket{algorithm="fixed_window",le="+Inf"} 1
		limigo_lua_execution_seconds_sum{algorithm="fixed_window"} 0.00004
		limigo_lua_execution_seconds_count{algorithm="fixed_window"} 1
	`)
	if err := testutil.CollectAndCompare(m.luaExecTime, expected, "limigo_lua_execution_seconds"); err != nil {
		t.Errorf("unexpected collecting result:\n%s", err)
	}
}

// TestRequestsTotalCardinality guards against unbounded series growth: N
// requests fanned out across a bounded set of rules and results must settle
// on exactly rules*results series, not one series per call. A future change
// that labels requests_total with something unbounded (an API key, an IP)
// would make this test's series count grow with N instead of staying fixed.
func TestRequestsTotalCardinality(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rules := []string{"free-tier", "pro-tier", "burst-tier"}
	results := []string{"allowed", "denied", "error", "unmatched"}

	const callsPerCombination = 25
	for i := 0; i < callsPerCombination; i++ {
		for _, rule := range rules {
			for _, result := range results {
				m.RecordRequest(rule, result)
			}
		}
	}

	want := len(rules) * len(results)
	got, err := testutil.GatherAndCount(reg, "limigo_requests_total")
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if got != want {
		t.Errorf("limigo_requests_total series count = %d, want %d (bounded by %d rules x %d results, independent of %d calls)",
			got, want, len(rules), len(results), callsPerCombination*len(rules)*len(results))
	}
}
