// Package metrics provides Prometheus instrumentation for Limigo. It owns a
// dedicated prometheus.Registry and exposes semantic recorder methods so
// callers never touch label names or ordering directly.
package metrics

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets are histogram boundaries tuned for sub-millisecond Redis/Lua
// calls. Prometheus's default buckets start at 5ms, which would collapse every
// observation into the smallest bucket and make percentiles meaningless.
var latencyBuckets = []float64{
	0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25,
}

// Metrics holds every Prometheus collector Limigo exposes, registered against
// a single owned registry.
type Metrics struct {
	reg                  *prometheus.Registry
	requestsTotal        *prometheus.CounterVec
	configReloadTotal    *prometheus.CounterVec
	tokenBucketFillRatio *prometheus.GaugeVec
	redisLatency         *prometheus.HistogramVec
	luaExecTime          *prometheus.HistogramVec
}

// New creates and registers all Limigo metrics against reg.
func New(reg *prometheus.Registry) (*Metrics, error) {
	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "limigo_requests_total",
			Help: "Total check requests by outcome and rule.",
		}, []string{"result", "rule"},
	)
	if err := reg.Register(requestsTotal); err != nil {
		return nil, fmt.Errorf("register requests_total: %w", err)
	}

	configReloadTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "limigo_config_reload_total",
			Help: "Total config hot-reload attempts by outcome.",
		}, []string{"result"},
	)
	if err := reg.Register(configReloadTotal); err != nil {
		return nil, fmt.Errorf("register config_reload_total: %w", err)
	}

	tokenBucketFillRatio := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "limigo_token_bucket_fill_ratio",
			Help: "Mean fill ratio (0.0-1.0) of local-cache token buckets, per rule.",
		}, []string{"rule"},
	)
	if err := reg.Register(tokenBucketFillRatio); err != nil {
		return nil, fmt.Errorf("register token_bucket_fill_ratio: %w", err)
	}

	redisLatency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "limigo_redis_latency_seconds",
			Help:    "Round-trip latency of Redis calls, by algorithm.",
			Buckets: latencyBuckets,
		}, []string{"algorithm"},
	)
	if err := reg.Register(redisLatency); err != nil {
		return nil, fmt.Errorf("register redis_latency_seconds: %w", err)
	}

	luaExecTime := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "limigo_lua_execution_seconds",
			Help:    "Server-side execution time of Lua scripts, by algorithm.",
			Buckets: latencyBuckets,
		}, []string{"algorithm"},
	)
	if err := reg.Register(luaExecTime); err != nil {
		return nil, fmt.Errorf("register lua_execution_seconds: %w", err)
	}

	return &Metrics{
		reg:                  reg,
		requestsTotal:        requestsTotal,
		configReloadTotal:    configReloadTotal,
		tokenBucketFillRatio: tokenBucketFillRatio,
		redisLatency:         redisLatency,
		luaExecTime:          luaExecTime,
	}, nil
}

// RecordRequest records a single check outcome for rule.
func (m *Metrics) RecordRequest(rule, result string) {
	m.requestsTotal.WithLabelValues(result, rule).Inc()
}

// ObserveRedisLatency records the full client-observed round-trip time of a
// Redis call for algorithm.
func (m *Metrics) ObserveRedisLatency(algorithm string, duration time.Duration) {
	m.redisLatency.WithLabelValues(algorithm).Observe(duration.Seconds())
}

// ObserveLuaExecution records the server-side execution time reported by the
// Lua script itself for algorithm, isolating script cost from network cost.
func (m *Metrics) ObserveLuaExecution(algorithm string, duration time.Duration) {
	m.luaExecTime.WithLabelValues(algorithm).Observe(duration.Seconds())
}

// SetTokenBucketFillRatio sets the current mean fill ratio for rule's
// local-cache token buckets.
func (m *Metrics) SetTokenBucketFillRatio(rule string, ratio float64) {
	m.tokenBucketFillRatio.WithLabelValues(rule).Set(ratio)
}

// ClearTokenBucketFillRatio removes rule's fill-ratio series entirely, so
// Grafana shows a gap instead of a misleading flat zero when no buckets exist.
func (m *Metrics) ClearTokenBucketFillRatio(rule string) {
	m.tokenBucketFillRatio.DeleteLabelValues(rule)
}

// IncConfigReload records the outcome of a hot-reload attempt.
func (m *Metrics) IncConfigReload(success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	m.configReloadTotal.WithLabelValues(result).Inc()
}

// Handler returns an http.Handler that serves this Metrics' registry in the
// Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
