# Limigo — Project Context for Claude Code

## What is this?

Limigo is a distributed, horizontally-scalable rate limiting service written in Go.
It is built as a portfolio project targeting full-stack/backend engineering roles in 2026.
The goal is production-grade quality: correct distributed semantics, real observability, and clean Go idioms.

## Core goals

- Demonstrate deep Go knowledge: interfaces, goroutines, channels, context propagation
- Show distributed systems thinking: consistency, atomic operations, failure modes
- First-class observability: metrics, tracing, dashboards out of the box
- Operationally mature: hot-reload config, graceful shutdown, structured logging

---

## Architecture

```
API clients
    │
    ▼
Rate limiter cluster (N nodes, stateless)
    │   ├── Node A
    │   ├── Node B
    │   └── Node C
    │
    ▼
Redis cluster (shared counter state — atomic via Lua scripts)
    │
    ▼
Observability layer
    ├── Prometheus (metrics endpoint)
    ├── Grafana (pre-built dashboard JSON)
    └── OpenTelemetry (distributed tracing)
    │
    ▼
Admin API (gRPC + REST)
    └── Hot-reload rules, inspect quotas, override limits
```

---

## Rate limiting algorithms

Implement all three behind a common Go interface (`Limiter`):

| Algorithm | Notes |
|---|---|
| **Token bucket** | Smooth burst allowance, refills at fixed rate |
| **Sliding window log** | Most accurate, higher memory cost |
| **Fixed window counter** | Simplest, susceptible to boundary bursts |

Callers select an algorithm per rule in config.

---

## Tech stack

| Layer | Choice |
|---|---|
| Language | Go (latest stable) |
| Shared state | Redis (single node dev, Redis Cluster prod) |
| Atomicity | Lua scripts executed server-side via `go-redis` |
| Metrics | `prometheus/client_golang` |
| Tracing | OpenTelemetry SDK for Go |
| Config | YAML/TOML, hot-reloaded via `fsnotify` |
| Admin API | gRPC (primary) + REST gateway |
| Load testing | `vegeta` or `k6` |
| Containerisation | Docker + Docker Compose for local dev |

---

## Key design decisions

### Atomic Redis operations
Never do a GET + SET for counters — race condition. All counter logic runs inside a
Lua script that executes atomically on the Redis server. This is the correct production
approach and should be highlighted in the README.

### Local cache with batched Redis writes
Each node maintains an in-process cache (sync.Map or similar) to absorb request bursts
locally. A background goroutine on a ticker (e.g. every 10ms) flushes deltas to Redis.
This trades a small accuracy window for significantly lower latency and Redis load.
The trade-off should be configurable and documented.

### Rule configuration
Rules are defined in a config file, e.g.:

```yaml
rules:
  - name: free-tier
    match:
      header: "X-Plan"
      value: "free"
    limit: 100
    window: 60s
    algorithm: sliding_window

  - name: pro-tier
    match:
      header: "X-Plan"
      value: "pro"
    limit: 10000
    window: 60s
    algorithm: token_bucket
```

Rules hot-reload on file change — no restart needed.

### Graceful shutdown
On SIGTERM: stop accepting new requests, drain in-flight requests, flush local cache
to Redis, then exit. Use `context.Context` propagation throughout.

---

## Observability

### Prometheus metrics to expose
- `limigo_requests_total{result="allowed|denied", rule="..."}` — counter
- `limigo_token_bucket_fill_ratio{rule="..."}` — gauge (0.0–1.0)
- `limigo_redis_latency_seconds` — histogram
- `limigo_lua_execution_seconds` — histogram
- `limigo_config_reload_total` — counter

### Grafana
Include a `grafana/dashboard.json` in the repo that can be imported directly.
Panels: req/s allowed vs denied, p50/p95/p99 Redis latency, fill ratio per rule.

### OpenTelemetry
Instrument the critical path: inbound request → rule match → Redis round-trip → response.
Export to a local Jaeger instance in the Docker Compose setup.

---

## Project structure (target)

```
limigo/
├── cmd/
│   └── limigo/             # main entrypoint
├── internal/
│   ├── limiter/            # algorithm implementations + Limiter interface
│   ├── store/              # Redis client + Lua scripts
│   ├── config/             # YAML parsing + fsnotify hot-reload
│   ├── api/                # gRPC + REST handlers
│   ├── metrics/            # Prometheus instrumentation
│   └── tracing/            # OpenTelemetry setup
├── scripts/
│   └── lua/                # Lua scripts for atomic Redis ops
├── grafana/
│   └── dashboard.json
├── docker-compose.yml
├── Dockerfile
├── config.example.yaml
├── LICENSE                 # MIT
├── CLAUDE.md               # this file
└── README.md
```

---

## README must include

- Problem statement: why distributed rate limiting is hard (race conditions, clock skew, Redis failover)
- Algorithm comparison table with trade-offs
- Architecture diagram
- Quickstart (Docker Compose up in one command)
- Load test results: measured numbers only, each with hardware and a reproduction
  command — no target figure is asserted here; see the README's `## Benchmarks`
  section and `docs/adr/0002-publish-measured-numbers-retire-the-50k-target.md`
- Design decisions section (Lua atomicity, local cache batching)

---

## Coding standards

- All public functions and types must have godoc comments
- Errors wrapped with `fmt.Errorf("...: %w", err)` — never discarded
- No global state — dependency inject everything
- Table-driven tests for all algorithm implementations
- Benchmarks (`_test.go` with `Benchmark*`) for the hot path
- `golangci-lint` must pass with default ruleset

---

## License

MIT — see `LICENSE` file in repo root.