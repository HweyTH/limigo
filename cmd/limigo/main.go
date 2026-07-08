package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hweyth/limigo/internal/config"
	"github.com/hweyth/limigo/internal/store"
	goredis "github.com/redis/go-redis/v9"
)

type options struct {
	configPath string
	redisAddr  string
}

type luaScripts struct {
	fixedWindow   string
	slidingWindow string
	tokenBucket   string
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "limigo: %v\n", err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

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

	scripts, err := loadLuaScripts(filepath.Join("internal", "store", "lua"))
	if err != nil {
		return fmt.Errorf("load Lua scripts: %w", err)
	}
	redisStore := store.NewRedisStore(redisClient, scripts.fixedWindow, scripts.slidingWindow, scripts.tokenBucket)
	_ = redisStore

	if _, err := fmt.Fprintf(stdout, "loaded %d rules from %s; redis address %s\n", len(cfg.Rules), opts.configPath, redisClient.Options().Addr); err != nil {
		return fmt.Errorf("write startup summary: %w", err)
	}
	return nil
}

func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (options, error) {
	opts := options{}
	flags := flag.NewFlagSet("limigo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.configPath, "config", envOrDefault(getenv, "LIMIGO_CONFIG", "config.example.yaml"), "path to Limigo YAML config file")
	flags.StringVar(&opts.redisAddr, "redis-addr", envOrDefault(getenv, "REDIS_ADDR", "localhost:6379"), "Redis server address")

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
	return opts, nil
}

func envOrDefault(getenv func(string) string, key string, fallback string) string {
	if value := getenv(key); strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func readTextFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file %q: %w", path, err)
	}
	return string(data), nil
}

func loadLuaScripts(dir string) (luaScripts, error) {
	fixedWindow, err := readTextFile(filepath.Join(dir, "fixed_window.lua"))
	if err != nil {
		return luaScripts{}, err
	}

	slidingWindow, err := readTextFile(filepath.Join(dir, "sliding_window.lua"))
	if err != nil {
		return luaScripts{}, err
	}

	tokenBucket, err := readTextFile(filepath.Join(dir, "token_bucket.lua"))
	if err != nil {
		return luaScripts{}, err
	}

	return luaScripts{
		fixedWindow:   fixedWindow,
		slidingWindow: slidingWindow,
		tokenBucket:   tokenBucket,
	}, nil
}
