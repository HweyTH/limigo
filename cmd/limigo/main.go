// Command limigo loads rate-limit rules, connects to Redis, and prepares the
// Redis-backed limiter store used by the service runtime.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hweyth/limigo/internal/api"
	"github.com/hweyth/limigo/internal/config"
	"github.com/hweyth/limigo/internal/metrics"
	"github.com/hweyth/limigo/internal/rules"
	"github.com/hweyth/limigo/internal/store"
	"github.com/hweyth/limigo/internal/tracing"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type options struct {
	configPath string
	redisAddr  string
	// redisClusterAddrs, when non-empty, selects a Redis Cluster client seeded
	// with these addresses instead of a standalone client at redisAddr. The two
	// are mutually exclusive: parseOptions rejects a cluster address list given
	// alongside an explicit redisAddr.
	redisClusterAddrs  []string
	httpAddr           string
	metricsAddr        string
	shutdownTimeout    time.Duration
	cacheFlushInterval time.Duration
	// drainDelay is how long the node keeps serving after failing /readyz and
	// before it stops accepting connections: the window in which the proxy's
	// next health check observes the 503 and routes new requests elsewhere.
	drainDelay time.Duration
	// tracingEndpoint is the OTLP/HTTP collector URL. Empty — the default —
	// means tracing is off and no span code runs on the request path.
	tracingEndpoint string
	// tracingSampleRatio is the fraction of new traces recorded when tracing
	// is on; traces with a sampled parent are always recorded.
	tracingSampleRatio float64
}

// serverTimeouts bounds how long an http.Server waits on a client at each
// stage of a connection. net/http treats a zero as "no timeout", so leaving
// any of these unset lets a client that sends headers slowly hold a
// goroutine and a file descriptor indefinitely — the slowloris shape, on a
// service whose job is to protect other services from abusive traffic.
type serverTimeouts struct {
	readHeader time.Duration
	read       time.Duration
	write      time.Duration
	idle       time.Duration
}

// defaultServerTimeouts applies to both listeners. The header budget closes
// the slowloris hole; read and write are generous next to the tiny JSON
// bodies /v1/check exchanges. idle must outlast Traefik's default 90s
// idleConnTimeout towards backends: if limigo hung up an idle keep-alive
// first, the proxy would hit closed connections and the load-test harness
// would start paying reconnection cost for a reason unrelated to the limiter.
var defaultServerTimeouts = serverTimeouts{
	readHeader: 5 * time.Second,
	read:       10 * time.Second,
	write:      10 * time.Second,
	idle:       120 * time.Second,
}

// defaultCheckTimeout bounds the store call behind every /v1/check. It must
// sit inside defaultServerTimeouts.write with room for the response itself:
// once WriteTimeout has passed, the fail-closed 503 cannot be written and a
// hung Redis surfaces as a connection reset that no metric ever sees.
const defaultCheckTimeout = 5 * time.Second

// drainThenShutdown is the shutdown ordering that makes /readyz mean
// something: fail readiness first, keep serving for drainDelay so the proxy's
// next health check sees the 503 and stops sending new work here, and only
// then stop accepting connections and drain what is in flight. Without the
// wait the endpoint exists but changes nothing — the proxy would still be
// routing to this node at the moment it closed its listener.
func drainThenShutdown(readiness *api.Readiness, drainDelay time.Duration, shutdown func() error) error {
	readiness.Draining()
	time.Sleep(drainDelay)
	return shutdown()
}

// newServer builds an http.Server with every timeout set, so no listener is
// ever constructed with the zero-value "wait forever" defaults.
func newServer(addr string, handler http.Handler, timeouts serverTimeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: timeouts.readHeader,
		ReadTimeout:       timeouts.read,
		WriteTimeout:      timeouts.write,
		IdleTimeout:       timeouts.idle,
	}
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		logf(os.Stderr, "limigo: %v\n", err)
		os.Exit(1)
	}
}

// logf writes a diagnostic line, discarding any write error. Used on paths that
// have no better way to report a failing stdout or stderr than to carry on.
func logf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// run performs startup work with injectable process dependencies so tests can
// exercise flag parsing, environment defaults, and output without spawning a process.
func run(args []string, getenv func(string) string, stdout io.Writer, stderr io.Writer) (runErr error) {
	opts, err := parseOptions(args, getenv, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", opts.configPath, err)
	}
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("validate config %q: %w", opts.configPath, err)
	}

	redisClient, redisTarget := newRedisClient(opts)
	defer func() {
		if err := redisClient.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close Redis client: %w", err)
		}
	}()

	startupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := redisClient.Ping(startupCtx).Err(); err != nil {
		return fmt.Errorf("ping Redis at %s: %w", redisTarget, err)
	}

	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}

	// Tracing is off unless an endpoint is given. Off means tracer is nil and
	// the request path runs exactly the code the published benchmarks ran —
	// not a no-op span per request, which would still be a change.
	var tracerProvider trace.TracerProvider
	var tracer trace.Tracer
	if opts.tracingEndpoint != "" {
		provider, shutdownTracing, err := tracing.Setup(context.Background(), opts.tracingEndpoint, opts.tracingSampleRatio)
		if err != nil {
			return fmt.Errorf("set up tracing: %w", err)
		}
		defer func() {
			if err := shutdownTracing(context.Background()); err != nil && runErr == nil {
				runErr = fmt.Errorf("shut down tracing: %w", err)
			}
		}()
		tracerProvider = provider
		tracer = provider.Tracer(tracing.ServiceName)
	}

	fixedWindowScript, slidingWindowScript, tokenBucketScript, leakyBucketScript, fixedWindowSyncScript, tokenBucketSyncScript, err := store.LoadEmbeddedScripts()
	if err != nil {
		return fmt.Errorf("load Lua scripts: %w", err)
	}
	redisStore := store.NewRedisStore(redisClient, fixedWindowScript, slidingWindowScript, tokenBucketScript, leakyBucketScript, fixedWindowSyncScript, tokenBucketSyncScript, m, tracer)

	engine, err := rules.Compile(cfg, redisStore, m)
	if err != nil {
		return fmt.Errorf("compile rules: %w", err)
	}

	holder := rules.NewEngineHolder(engine)
	readiness := api.NewReadiness()

	mux := http.NewServeMux()
	checkHandler := api.NewCheckHandler(holder, m, defaultCheckTimeout, tracer)
	if tracerProvider != nil {
		// Only /v1/check is wrapped. /healthz is the control endpoint every
		// benchmark ceiling is measured against and must stay untouched, and
		// a health check has nothing to trace.
		checkHandler = otelhttp.NewHandler(checkHandler, "POST /v1/check",
			otelhttp.WithTracerProvider(tracerProvider),
			otelhttp.WithPropagators(propagation.TraceContext{}),
		)
	}
	mux.Handle("/v1/check", checkHandler)
	mux.Handle("/healthz", api.NewHealthzHandler())
	mux.Handle("/readyz", readiness.Handler())

	server := newServer(opts.httpAddr, mux, defaultServerTimeouts)

	var metricsServer *http.Server
	if opts.metricsAddr != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", m.Handler())
		metricsServer = newServer(opts.metricsAddr, metricsMux, defaultServerTimeouts)
	}

	tracingSummary := "tracing off"
	if opts.tracingEndpoint != "" {
		tracingSummary = fmt.Sprintf("tracing to %s (sample ratio %v)", opts.tracingEndpoint, opts.tracingSampleRatio)
	}
	if _, err := fmt.Fprintf(stdout, "loaded %d rules from %s; redis %s; HTTP address %s; %s\n", len(cfg.Rules), opts.configPath, redisTarget, opts.httpAddr, tracingSummary); err != nil {
		return fmt.Errorf("write startup summary: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		reloadFailed := func(err error) {
			m.IncConfigReload(false)
			logf(stderr, "limigo: config reload failed: %v\n", err)
		}
		onChange := func() {
			newCfg, err := config.Load(opts.configPath)
			if err != nil {
				reloadFailed(err)
				return
			}
			if err := config.Validate(newCfg); err != nil {
				reloadFailed(err)
				return
			}
			newEngine, err := rules.Compile(newCfg, redisStore, m)
			if err != nil {
				reloadFailed(err)
				return
			}
			// holder still points at the outgoing engine here, so this reconciles
			// its pending local_cache admits with Redis before the swap makes them
			// unreachable.
			if err := holder.FlushLocalCaches(ctx); err != nil {
				logf(stderr, "limigo: flush local caches before reload failed: %v\n", err)
			}
			holder.Store(newEngine)
			m.IncConfigReload(true)
			logf(stdout, "config reloaded: %d rules from %s\n", len(newCfg.Rules), opts.configPath)
		}
		if err := config.Watch(ctx, opts.configPath, onChange); err != nil {
			logf(stderr, "limigo: config watcher stopped: %v\n", err)
		}
	}()

	cacheFlushTicker := time.NewTicker(opts.cacheFlushInterval)
	go func() {
		defer cacheFlushTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-cacheFlushTicker.C:
				if err := holder.FlushLocalCaches(ctx); err != nil {
					logf(stderr, "limigo: local cache flush failed: %v\n", err)
				}
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	var metricsErr chan error
	if metricsServer != nil {
		metricsErr = make(chan error, 1)
		go func() {
			metricsErr <- metricsServer.ListenAndServe()
		}()
	}

	// The listener binds inside ListenAndServe, a few microseconds after this
	// line; a health check that lands in that gap gets a refused connection,
	// which reads as not-ready anyway.
	readiness.Ready()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP on %q: %w", opts.httpAddr, err)
		}
		return nil
	case err := <-metricsErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve metrics on %q: %w", opts.metricsAddr, err)
		}
		return nil
	case <-ctx.Done():
		if _, err := fmt.Fprintf(stdout, "received shutdown signal, failing readiness and serving for %s before draining in-flight requests (timeout %s)...\n", opts.drainDelay, opts.shutdownTimeout); err != nil {
			return fmt.Errorf("write shutdown notice: %w", err)
		}

		// The drain delay is not charged against the shutdown timeout: the
		// timeout bounds the drain of in-flight requests, which only starts
		// once the listener closes.
		var shutdownCtx context.Context
		var shutdownCancel context.CancelFunc
		err := drainThenShutdown(readiness, opts.drainDelay, func() error {
			shutdownCtx, shutdownCancel = context.WithTimeout(context.Background(), opts.shutdownTimeout)
			return server.Shutdown(shutdownCtx)
		})
		defer shutdownCancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("graceful shutdown timed out after %s: %w", opts.shutdownTimeout, err)
			}
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}

		if metricsServer != nil {
			if err := metricsServer.Shutdown(shutdownCtx); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("graceful shutdown of metrics server timed out after %s: %w", opts.shutdownTimeout, err)
				}
				return fmt.Errorf("shutdown metrics server: %w", err)
			}
		}

		if err := holder.FlushLocalCaches(shutdownCtx); err != nil {
			return fmt.Errorf("flush local caches during shutdown: %w", err)
		}

		if _, err := fmt.Fprintf(stdout, "shutdown complete\n"); err != nil {
			return fmt.Errorf("write shutdown complete notice: %w", err)
		}
		return nil
	}
}

// newRedisClient builds the Redis client opts selects: a ClusterClient seeded
// with redisClusterAddrs when that list is set, otherwise a standalone Client
// at redisAddr. target describes the choice for log lines. Both client types
// satisfy redis.Scripter, which is all store.RedisStore needs — the Lua
// scripts are single-key and run unmodified on a cluster.
//
// ContextTimeoutEnabled is on for both: by default go-redis ignores the
// context's deadline for dial and read and applies only its own timeouts,
// across retries, so the per-request deadline /v1/check derives would not
// reach the socket. With it on, that deadline bounds the whole round-trip.
func newRedisClient(opts options) (client goredis.UniversalClient, target string) {
	if len(opts.redisClusterAddrs) > 0 {
		return goredis.NewClusterClient(&goredis.ClusterOptions{
			Addrs:                 opts.redisClusterAddrs,
			ContextTimeoutEnabled: true,
		}), fmt.Sprintf("cluster %s", strings.Join(opts.redisClusterAddrs, ","))
	}
	return goredis.NewClient(&goredis.Options{
		Addr:                  opts.redisAddr,
		ContextTimeoutEnabled: true,
	}), fmt.Sprintf("address %s", opts.redisAddr)
}

// parseOptions reads command-line flags and environment fallbacks into runtime options.
func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (options, error) {
	opts := options{}
	flags := flag.NewFlagSet("limigo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", envDurationOrDefault(getenv, "LIMIGO_SHUTDOWN_TIMEOUT", 10*time.Second), "max time to wait for in-flight requests to finish on shutdown")
	flags.DurationVar(&opts.cacheFlushInterval, "cache-flush-interval", envDurationOrDefault(getenv, "LIMIGO_CACHE_FLUSH_INTERVAL", 10*time.Millisecond), "how often to reconcile local_cache rules with the backing store")
	// Default covers one Traefik health-check interval (5s in
	// docker-compose.yml) with a second to spare for the check itself.
	flags.DurationVar(&opts.drainDelay, "drain-delay", envDurationOrDefault(getenv, "LIMIGO_DRAIN_DELAY", 6*time.Second), "how long to keep serving after failing /readyz before closing the listener; cover one load-balancer health-check interval")
	flags.StringVar(&opts.tracingEndpoint, "tracing-endpoint", envOrDefault(getenv, "LIMIGO_TRACING_ENDPOINT", ""), "OTLP/HTTP collector URL for traces, e.g. http://jaeger:4318; empty disables tracing")
	flags.Float64Var(&opts.tracingSampleRatio, "tracing-sample-ratio", envFloatOrDefault(getenv, "LIMIGO_TRACING_SAMPLE_RATIO", 1.0), "fraction of new traces to record when tracing is on, 0 to 1")
	flags.StringVar(&opts.configPath, "config", envOrDefault(getenv, "LIMIGO_CONFIG", "config.example.yaml"), "path to Limigo YAML config file")
	// redisAddr keeps a default so the common single-node case needs no flag;
	// that means "was it set?" cannot be read off the value alone. redisAddrSet
	// tracks the environment now and the flag after Parse (via flags.Visit).
	redisAddrSet := strings.TrimSpace(getenv("REDIS_ADDR")) != ""
	flags.StringVar(&opts.redisAddr, "redis-addr", envOrDefault(getenv, "REDIS_ADDR", "localhost:6379"), "Redis server address (standalone)")
	var redisClusterAddrs string
	flags.StringVar(&redisClusterAddrs, "redis-cluster-addrs", envOrDefault(getenv, "LIMIGO_REDIS_CLUSTER_ADDRS", ""), "comma-separated Redis Cluster seed addresses; mutually exclusive with -redis-addr")
	flags.StringVar(
		&opts.httpAddr,
		"http-addr",
		envOrDefault(getenv, "LIMIGO_HTTP_ADDR", ":8080"),
		"HTTP listen address",
	)
	flags.StringVar(
		&opts.metricsAddr,
		"metrics-addr",
		envOrDefault(getenv, "LIMIGO_METRICS_ADDR", ":9091"),
		"metrics listen address (prometheus /metrics); empty disables the listener",
	)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "redis-addr" {
			redisAddrSet = true
		}
	})
	opts.redisClusterAddrs = splitAddrs(redisClusterAddrs)
	if len(opts.redisClusterAddrs) > 0 && redisAddrSet {
		return opts, fmt.Errorf("redis address and redis cluster addresses are mutually exclusive; set one")
	}
	if strings.TrimSpace(opts.configPath) == "" {
		return opts, fmt.Errorf("config path must not be empty")
	}
	if strings.TrimSpace(opts.redisAddr) == "" {
		return opts, fmt.Errorf("redis address must not be empty")
	}
	if strings.TrimSpace(opts.httpAddr) == "" {
		return opts, fmt.Errorf("HTTP address must not be empty")
	}
	if opts.cacheFlushInterval <= 0 {
		return opts, fmt.Errorf("cache flush interval must be greater than zero")
	}
	if opts.drainDelay < 0 {
		return opts, fmt.Errorf("drain delay must not be negative")
	}
	if opts.tracingSampleRatio < 0 || opts.tracingSampleRatio > 1 {
		return opts, fmt.Errorf("tracing sample ratio must be between 0 and 1")
	}
	return opts, nil
}

// splitAddrs turns a comma-separated address list into its non-blank entries,
// trimmed, so "a:1, b:2," and "a:1,b:2" parse the same. An empty or
// all-blank list yields nil.
func splitAddrs(list string) []string {
	var addrs []string
	for addr := range strings.SplitSeq(list, ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// envOrDefault returns a non-blank environment value or the supplied fallback.
func envOrDefault(getenv func(string) string, key string, fallback string) string {
	if value := getenv(key); strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

// envFloatOrDefault returns a parseable environment value or the supplied fallback.
func envFloatOrDefault(getenv func(string) string, key string, fallback float64) float64 {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationOrDefault(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
