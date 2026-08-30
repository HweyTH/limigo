//go:build bench

package rules

// Engine + Redis benchmarks: a full rule evaluation — rule match,
// Lua execution, round trip — against a real Redis via testcontainers, so
// the store layer's cost is separable from the pure-algorithm cost measured
// in internal/limiter. This is the number that justifies the local-cache
// design existing at all: it's the round trip local caching trades accuracy
// to avoid.
//
// Real Redis is used rather than an in-memory fake: a fake
// does not execute Lua the way real Redis does, so it would measure Go
// function-call overhead and report it as Lua execution cost.
//
// These require Docker, so they're gated behind the bench build tag and
// excluded from the default `go test ./...` run. Run them with:
//
//	go test -tags bench -bench . ./internal/rules/...
//
// This file provisions its own container rather than reusing
// internal/store/redis_store_test.go's helpers: Go test helpers are
// unexported and compiled only into their own package's test binary, so
// they aren't importable across packages. It uses store.LoadEmbeddedScripts
// for Lua source instead of that file's os.ReadFile pattern, since the
// embedded loader has no working-directory dependency.

import (
	"context"
	"os"
	"testing"

	"github.com/hweyth/limigo/internal/config"
	"github.com/hweyth/limigo/internal/metrics"
	"github.com/hweyth/limigo/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// benchRedisClient is the Redis client shared by every benchmark in this
// file, backed by the single container TestMain provisions.
var benchRedisClient *goredis.Client

// TestMain provisions one Redis container for the whole benchmark binary
// run. Container startup (several seconds) happens here, once, before any
// benchmark's timer starts, so it never lands inside a measured region.
func TestMain(m *testing.M) {
	ctx := context.Background()

	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic("failed to start container: " + err.Error())
	}

	endpoint, err := redisContainer.Endpoint(ctx, "")
	if err != nil {
		panic("failed to get container endpoint: " + err.Error())
	}

	benchRedisClient = goredis.NewClient(&goredis.Options{Addr: endpoint})

	code := m.Run()

	benchRedisClient.Close()
	_ = redisContainer.Terminate(ctx)

	os.Exit(code)
}

// newBenchEngine compiles the rule engine from config.example.yaml's
// controlled A/B pair — burst-tier-token-bucket (uncached) and
// cached-tier-token-bucket (identical limit/rate, local_cache: true) — bound
// to the shared benchmark Redis, so cached and uncached cost are measured
// against the same store and the same algorithm parameters.
func newBenchEngine(b *testing.B) *Engine {
	b.Helper()

	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		b.Fatalf("load config: %v", err)
	}
	if err := config.Validate(cfg); err != nil {
		b.Fatalf("validate config: %v", err)
	}

	fixedWindowScript, slidingWindowScript, tokenBucketScript, leakyBucketScript, fixedWindowSyncScript, tokenBucketSyncScript, err := store.LoadEmbeddedScripts()
	if err != nil {
		b.Fatalf("load embedded lua scripts: %v", err)
	}

	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		b.Fatalf("init metrics: %v", err)
	}

	redisStore := store.NewRedisStore(benchRedisClient, fixedWindowScript, slidingWindowScript, tokenBucketScript, leakyBucketScript, fixedWindowSyncScript, tokenBucketSyncScript, m)

	engine, err := Compile(cfg, redisStore, m)
	if err != nil {
		b.Fatalf("compile engine: %v", err)
	}
	return engine
}

// BenchmarkEngineCheck_Uncached measures a full rule evaluation against real
// Redis for a rule with no local cache: every call executes the Lua script
// and pays a full round trip.
func BenchmarkEngineCheck_Uncached(b *testing.B) {
	runEngineCheckBench(b, "burst")
}

// BenchmarkEngineCheck_Cached measures a full rule evaluation for a rule with
// local caching enabled: Allow decides against the node-local cache with no
// Redis round trip, which is what this number should show relative to
// BenchmarkEngineCheck_Uncached.
func BenchmarkEngineCheck_Cached(b *testing.B) {
	runEngineCheckBench(b, "cached")
}

// runEngineCheckBench repeatedly checks a single key against the rule
// matched by the X-Plan value plan, using the shared benchmark engine.
func runEngineCheckBench(b *testing.B, plan string) {
	engine := newBenchEngine(b)
	ctx := context.Background()
	headerValue := func(string) string { return plan }

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = engine.Check(ctx, "bench-key", headerValue)
	}
}
