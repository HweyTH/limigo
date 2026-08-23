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
	"strings"
	"syscall"
	"time"

	"github.com/hweyth/limigo/internal/api"
	"github.com/hweyth/limigo/internal/config"
	"github.com/hweyth/limigo/internal/metrics"
	"github.com/hweyth/limigo/internal/rules"
	"github.com/hweyth/limigo/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
)

type options struct {
	configPath         string
	redisAddr          string
	httpAddr           string
	metricsAddr        string
	shutdownTimeout    time.Duration
	cacheFlushInterval time.Duration
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "limigo: %v\n", err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
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

	redisClient := goredis.NewClient(&goredis.Options{
		Addr: opts.redisAddr,
	})
	defer func() {
		if err := redisClient.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close Redis client: %w", err)
		}
	}()

	startupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := redisClient.Ping(startupCtx).Err(); err != nil {
		return fmt.Errorf("ping Redis at %q: %w", opts.redisAddr, err)
	}

	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}

	fixedWindowScript, slidingWindowScript, tokenBucketScript, leakyBucketScript, fixedWindowSyncScript, tokenBucketSyncScript, err := store.LoadEmbeddedScripts()
	if err != nil {
		return fmt.Errorf("load Lua scripts: %w", err)
	}
	redisStore := store.NewRedisStore(redisClient, fixedWindowScript, slidingWindowScript, tokenBucketScript, leakyBucketScript, fixedWindowSyncScript, tokenBucketSyncScript, m)

	engine, err := rules.Compile(cfg, redisStore, m)
	if err != nil {
		return fmt.Errorf("compile rules: %w", err)
	}

	holder := rules.NewEngineHolder(engine)

	mux := http.NewServeMux()
	mux.Handle("/v1/check", api.NewCheckHandler(holder, m))

	server := &http.Server{
		Addr:    opts.httpAddr,
		Handler: mux,
	}

	var metricsServer *http.Server
	if opts.metricsAddr != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", m.Handler())
		metricsServer = &http.Server{
			Addr:    opts.metricsAddr,
			Handler: metricsMux,
		}
	}

	if _, err := fmt.Fprintf(stdout, "loaded %d rules from %s; redis address %s; HTTP address %s\n", len(cfg.Rules), opts.configPath, redisClient.Options().Addr, opts.httpAddr); err != nil {
		return fmt.Errorf("write startup summary: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		onChange := func() {
			newCfg, err := config.Load(opts.configPath)
			if err != nil {
				m.IncConfigReload(false)
				fmt.Fprintf(stderr, "limigo: config reload failed: %v\n", err)
				return
			}
			if err := config.Validate(newCfg); err != nil {
				m.IncConfigReload(false)
				fmt.Fprintf(stderr, "limigo: config reload failed: %v\n", err)
				return
			}
			newEngine, err := rules.Compile(newCfg, redisStore, m)
			if err != nil {
				m.IncConfigReload(false)
				fmt.Fprintf(stderr, "limigo: config reload failed: %v\n", err)
				return
			}
			// Flush the outgoing engine's local caches before swapping it out —
			// holder still points at the old engine here, so this reconciles any
			// pending local_cache admits with Redis before they become unreachable.
			if err := holder.FlushLocalCaches(ctx); err != nil {
				fmt.Fprintf(stderr, "limigo: flush local caches before reload failed: %v\n", err)
			}
			holder.Store(newEngine)
			m.IncConfigReload(true)
			fmt.Fprintf(stdout, "config reloaded: %d rules from %s\n", len(newCfg.Rules), opts.configPath)
		}
		if err := config.Watch(ctx, opts.configPath, onChange); err != nil {
			fmt.Fprintf(stderr, "limigo: config watcher stopped: %v\n", err)
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
					fmt.Fprintf(stderr, "limigo: local cache flush failed: %v\n", err)
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
		if _, err := fmt.Fprintf(stdout, "received shutdown signal, draining in-flight requests (timeout %s)...\n", opts.shutdownTimeout); err != nil {
			return fmt.Errorf("write shutdown notice: %w", err)
		}

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
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

// parseOptions reads command-line flags and environment fallbacks into runtime options.
func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (options, error) {
	opts := options{}
	flags := flag.NewFlagSet("limigo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", envDurationOrDefault(getenv, "LIMIGO_SHUTDOWN_TIMEOUT", 10*time.Second), "max time to wait for in-flight requests to finish on shutdown")
	flags.DurationVar(&opts.cacheFlushInterval, "cache-flush-interval", envDurationOrDefault(getenv, "LIMIGO_CACHE_FLUSH_INTERVAL", 10*time.Millisecond), "how often to reconcile local_cache rules with the backing store")
	flags.StringVar(&opts.configPath, "config", envOrDefault(getenv, "LIMIGO_CONFIG", "config.example.yaml"), "path to Limigo YAML config file")
	flags.StringVar(&opts.redisAddr, "redis-addr", envOrDefault(getenv, "REDIS_ADDR", "localhost:6379"), "Redis server address")
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
	if strings.TrimSpace(opts.configPath) == "" {
		return opts, fmt.Errorf("config path must not be empty")
	}
	if strings.TrimSpace(opts.redisAddr) == "" {
		return opts, fmt.Errorf("Redis address must not be empty")
	}
	if strings.TrimSpace(opts.httpAddr) == "" {
		return opts, fmt.Errorf("HTTP address must not be empty")
	}
	if opts.cacheFlushInterval <= 0 {
		return opts, fmt.Errorf("cache flush interval must be greater than zero")
	}
	return opts, nil
}

// envOrDefault returns a non-blank environment value or the supplied fallback.
func envOrDefault(getenv func(string) string, key string, fallback string) string {
	if value := getenv(key); strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
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
