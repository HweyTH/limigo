# Guide: Prometheus + Grafana Observability for Limigo

This guide explains, from first principles, how the Prometheus + Grafana integration works —
the concepts, the Go/Lua/PromQL syntax, and the reasoning behind each design decision. Read it
before (or alongside) implementing the plan in this repo's planning docs, so you understand
*why* each piece exists, not just what to type.

## Table of contents

1. [The one-paragraph mental model](#1-the-one-paragraph-mental-model)
2. [What a metric actually is](#2-what-a-metric-actually-is)
3. [The four metric types (and why each fits)](#3-the-four-metric-types-and-why-each-fits)
4. [Histograms — the concept most people find fuzzy](#4-histograms--the-concept-most-people-find-fuzzy)
5. [The `internal/metrics` package — syntax walk-through](#5-the-internalmetrics-package--syntax-walk-through)
6. [Dependency injection via narrow interfaces](#6-dependency-injection-via-narrow-interfaces)
7. [The Lua `TIME` trick — measuring the right thing](#7-the-lua-time-trick--measuring-the-right-thing)
8. [Instrumenting the request handler — the 4-way outcome](#8-instrumenting-the-request-handler--the-4-way-outcome)
9. [The fill-ratio gauge — reusing work you already do](#9-the-fill-ratio-gauge--reusing-work-you-already-do)
10. [Wiring it in `main.go` + the second listener](#10-wiring-it-in-maingo--the-second-listener)
11. [The Docker glue](#11-the-docker-glue-how-the-three-programs-find-each-other)
12. [How we test it](#12-how-we-test-it)
13. [Decisions log](#13-decisions-log)

---

## 1. The one-paragraph mental model

Your Go app keeps a handful of **numbers in memory** (how many requests were allowed, how long
Redis took, etc.). It exposes them as plain text at a URL: `GET /metrics`. **Prometheus** is a
separate program that visits that URL every few seconds, copies the numbers down, and remembers
them *with timestamps* — building a history. **Grafana** is a third program that asks Prometheus
"give me the allowed-request rate over the last hour" and draws it as a graph. Your app doesn't
push anything or draw anything; it just exposes current numbers and lets Prometheus pull them.

```
Your Go app                Prometheus                 Grafana
(holds live numbers)  ──▶  (scrapes every 5s,   ──▶  (queries + draws
 GET /metrics text         stores history)            dashboards)
        ▲                        │                         │
        └──── pull ──────────────┘                         │
                                 └──── query (PromQL) ──────┘
```

The key word is **pull**. This is why there's no "send metric" code — you only ever *increment a
counter in RAM*, and Prometheus does the fetching.

---

## 2. What a metric actually is

A Prometheus metric is a **named number that changes over time**, optionally split by **labels**.
Example of what your `/metrics` endpoint will literally output as text:

```
limigo_requests_total{result="allowed",rule="free-tier-fixed-window"} 4021
limigo_requests_total{result="denied",rule="free-tier-fixed-window"} 87
limigo_requests_total{result="error",rule="burst-tier-token-bucket"} 3
```

- `limigo_requests_total` — the **metric name**.
- `{result="...",rule="..."}` — **labels**. Each unique combination of label values is its own
  independent counter, called a **time series**.
- The number at the end — the current value.

**Labels are the superpower and the footgun.** They let you slice ("show denied requests for the
free tier"). But every distinct label combination is a separate series stored forever, so labels
must have a **small, bounded set of values**. `result` has 4 values, `rule` has however many rules
you configured — bounded. If you ever put the *client's API key* or *IP address* in a label, you'd
get millions of series and melt Prometheus. This is called **cardinality**, and it's the #1 way
real Prometheus setups die. That's why the test suite includes a "cardinality guard" (§12).

---

## 3. The four metric types (and why each fits)

| Type | Behavior | Where we use it | Why |
|---|---|---|---|
| **Counter** | Only ever goes **up** (resets to 0 on restart) | `requests_total`, `config_reload_total` | You count *events that happened*. You never "un-happen" a request. |
| **Gauge** | Goes **up and down**, holds a current value | `token_bucket_fill_ratio` | A bucket's fill level is a snapshot that rises and falls. |
| **Histogram** | Bucketed distribution of measurements | `redis_latency_seconds`, `lua_execution_seconds` | "How long did it take?" isn't one number — you want p50/p95/p99. |

**Why a counter for requests instead of a gauge?** Because you don't graph the raw count (it just
climbs forever) — you graph its **rate of change**: `rate(limigo_requests_total[1m])` = "requests
per second right now." Rate-of-a-counter is the fundamental Prometheus pattern.

---

## 4. Histograms — the concept most people find fuzzy

You can't average latencies meaningfully (one 500ms request hides behind a thousand 1ms ones).
Instead, a histogram pre-defines **buckets** and counts how many observations fall below each
boundary.

When you write `ObserveRedisLatency("token_bucket", 0.003)` (3ms), the histogram increments the
count of every bucket whose boundary is ≥ 0.003. On `/metrics` it exposes:

```
limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.001"} 12
limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.0025"} 40
limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.005"} 95   ← the 3ms one lands here
limigo_redis_latency_seconds_bucket{algorithm="token_bucket",le="0.01"} 98
...
limigo_redis_latency_seconds_sum   0.412      (total of all observed values)
limigo_redis_latency_seconds_count 98         (how many observations)
```

`le` means "less than or equal." Grafana then reconstructs a percentile with
**`histogram_quantile`**:

```promql
histogram_quantile(0.99, sum(rate(limigo_redis_latency_seconds_bucket[5m])) by (le))
```

Read inside-out: `rate(..._bucket[5m])` = per-second rate of each bucket, `sum by (le)` = combine
across algorithms keeping the bucket boundary, `histogram_quantile(0.99, …)` = "the latency below
which 99% of requests fell." That's your p99.

**Why we override the default buckets:** Prometheus's default buckets start at 5ms. Your Redis
calls are *sub-millisecond*, so with defaults every single request would land in the smallest
bucket and every percentile would read "≤5ms" — a flat, useless line. Our custom buckets start at
50µs (`0.00005`) so you get real resolution where your latencies actually live:

```go
Buckets: []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25}
```

---

## 5. The `internal/metrics` package — syntax walk-through

### The struct + constructor

```go
type Metrics struct {
    reg           *prometheus.Registry
    requestsTotal *prometheus.CounterVec   // the "Vec" = has labels
    redisLatency  *prometheus.HistogramVec
    // ... etc
}

func New(reg *prometheus.Registry) (*Metrics, error) {
    requestsTotal := prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "limigo_requests_total",
            Help: "Total check requests by outcome and rule.", // shows in /metrics
        },
        []string{"result", "rule"}, // ← the label names, in order
    )
    if err := reg.Register(requestsTotal); err != nil {
        return nil, fmt.Errorf("register requests_total: %w", err)
    }
    // ... repeat for each metric ...
    return &Metrics{reg: reg, requestsTotal: requestsTotal /* ... */}, nil
}
```

Things to notice:

- **`CounterVec` vs `Counter`**: the `Vec` suffix means "this metric has labels" — it's really a
  *family* of counters, one per label combination. You get a specific one with
  `.WithLabelValues("allowed", "free-tier")`.
- **`reg.Register(...)` returns an error** instead of panicking. We deliberately avoid the
  `promauto` package (which panics on a duplicate registration) because this project forbids
  `panic` in app code. We wrap the error with `%w` — standard Go error-chaining.
- **Custom registry**: `reg := prometheus.NewRegistry()` instead of the global default. This
  follows the "no global state" rule — each `Metrics` owns its registry, which makes tests fully
  isolated (a fresh registry per test = no cross-test pollution).

### The semantic methods (why not expose the raw metric?)

```go
func (m *Metrics) RecordRequest(rule, result string) {
    m.requestsTotal.WithLabelValues(result, rule).Inc()
}
func (m *Metrics) ObserveRedisLatency(algorithm string, d time.Duration) {
    m.redisLatency.WithLabelValues(algorithm).Observe(d.Seconds())
}
```

We deliberately **don't** let `redis.go` call `m.requestsTotal.WithLabelValues(...)` directly.
Why? Because `WithLabelValues` takes positional strings — if a caller passes
`("free-tier", "allowed")` in the wrong order, or typos `"denyed"`, you silently get a *new wrong
time series* instead of a compile error. Funnelling every write through one method per metric
means the label order and spelling live in exactly one place. This is the difference between
"added metrics for the checkbox" and "designed an instrumentation layer."

Note `d.Seconds()` — Prometheus convention is **always seconds** for time (hence the `_seconds`
suffix), even though Go's natural unit is `time.Duration` (nanoseconds).

---

## 6. Dependency injection via narrow interfaces

This codebase already does this pattern: `internal/store/store.go` defines tiny interfaces like
`FixedWindowStore` describing *just* what a consumer needs. The metrics wiring follows the same
pattern so that `store`, `api`, and `rules` **don't import the `metrics` package at all**. Instead,
each declares the slice of behavior it needs:

```go
// in package store — store defines what IT needs, not what metrics provides
type LatencyRecorder interface {
    ObserveRedisLatency(algorithm string, d time.Duration)
    ObserveLuaExecution(algorithm string, d time.Duration)
}
```

Then `NewRedisStore(..., recorder LatencyRecorder)` accepts anything with those two methods. Your
`*metrics.Metrics` happens to have them, so it satisfies the interface **structurally** — Go
doesn't need an `implements` keyword; if the methods match, it fits. This is **dependency
inversion**: the low-level `store` package defines the contract, and the high-level `main` wires
the concrete type in. Benefits:

- `store` stays decoupled (no `metrics` import → no import cycle risk).
- Tests can pass a 5-line fake recorder instead of a real Prometheus registry.

Same idea for `api.RequestRecorder` and `rules.FillRatioRecorder`. `main` is the only package that
touches the concrete `*metrics.Metrics`, because `main`'s job is wiring.

---

## 7. The Lua `TIME` trick — measuring the right thing

Here's the subtle one. When `redis.go` does `script.Run(...)`, from Go's side you can only time
the **whole round trip**: network there + Redis runs the script + network back. You *cannot*
separate "network was slow" from "script was slow" — they're one blocking call.

So we ask Redis to time *itself* from the inside. Redis's `TIME` command returns
`{seconds, microseconds}`. The script samples it at entry and exit and returns the delta as an
extra value:

```lua
local t0 = redis.call('TIME')          -- {seconds, microseconds}, at start

-- ... the real rate-limit logic (INCR, HGET, etc.) ...

local t1 = redis.call('TIME')          -- at end, right before return
local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])   -- microseconds elapsed
return {allowed, lua_us}               -- was: return allowed
```

Now Go gets **two honest numbers**:

- `redis_latency_seconds` = the full round trip Go measured (network + script).
- `lua_execution_seconds` = the `lua_us` the script reported (script only).

Subtract them and you know your network overhead. When latency spikes in production, you
instantly know whether to blame the network or the script — instead of shrugging.

Analogy: Redis does two things when you call it — run over the network (slow, far away) and run
the Lua script (fast, inside Redis). Timing only from outside is like tasting soup and asking "how
much salt vs. how much pepper?" — it's all mixed together. Asking Redis to report its own clock
delta is like asking the chef directly how long they spent on each ingredient.

**The important safety detail:** the Go method *signatures don't change*. `AllowFixedWindow` still
returns `(bool, error)`. Only the *internal parsing* changes — from `.Int()` (read one number) to
`.Int64Slice()` (read `[allowed, lua_us]` and use element 0). Because the public contract is
stable, existing integration tests in `redis_store_test.go` keep passing unchanged.

One parsing wrinkle: `token_bucket_sync.lua` already returns a *string* (to preserve fractional
tokens). Wrapped as `{tostring(tokens), lua_us}` it becomes a mixed array (string + int), so its
one method parses via `.Slice()` and converts each element by hand. It's the only method that's
slightly awkward.

### Return contract changes, per script

| Script | Old return | New return |
|---|---|---|
| `fixed_window.lua` | `1`/`0` | `{allowed, lua_us}` |
| `sliding_window.lua` | `1`/`0` | `{allowed, lua_us}` |
| `token_bucket.lua` | `allowed` | `{allowed, lua_us}` |
| `leaky_bucket.lua` | `{allowed, retry_ms}` | `{allowed, retry_ms, lua_us}` |
| `fixed_window_sync.lua` | `count` | `{count, lua_us}` |
| `token_bucket_sync.lua` | `tostring(tokens)` | `{tostring(tokens), lua_us}` |

---

## 8. Instrumenting the request handler — the 4-way outcome

In `check.go`, right after the engine decides, we classify into exactly one of four buckets and
record it:

```go
decision, err := engine.Check(r.Context(), key, r.Header.Get)
switch {
case err != nil:
    rec.RecordRequest(decision.RuleName, "error")     // store/Redis broke → we fail closed
case !decision.Matched:
    rec.RecordRequest("", "unmatched")                // no rule applied to this request
case decision.Allowed:
    rec.RecordRequest(decision.RuleName, "allowed")
default:
    rec.RecordRequest(decision.RuleName, "denied")    // a rule matched and throttled it
}
```

Why four values and not just `allowed`/`denied`? Because **`error` and `denied` are operationally
opposite**. `denied` = the rate limiter working as designed (Tuesday). `error` = Redis is down and
you're fail-closing everyone (an incident, page someone). If you collapse them, your "denials"
graph spikes during an outage and you can't tell a real attack from a broken dependency. Splitting
them gives you a dedicated "fail-closed rate" panel. `unmatched` is separated for the same reason
— "traffic nobody wrote a rule for" is its own signal.

Early rejects that never reach the engine (bad method, bad body, empty key) are **not** recorded —
they're malformed requests, not rate-limit decisions.

---

## 9. The fill-ratio gauge — reusing work you already do

`token_bucket_fill_ratio` answers "how full are the buckets for this rule, on average, right now?"
The trick is *where* to compute it cheaply. The app already runs a background goroutine every 10ms
that flushes local-cache buckets to Redis — and that flush **already iterates every bucket** via
`manager.Snapshot()`. So we piggyback: while it's iterating, sum up each bucket's fill ratio and
set the gauge.

First, the bucket needs to be able to report its own fill level (it currently can't):

```go
// in batching_token_bucket.go
func (b *BatchingTokenBucket) FillRatio() float64 {
    b.mu.Lock()
    defer b.mu.Unlock()
    if b.capacity <= 0 {
        return 0 // guard: never divide by zero → no NaN in metrics
    }
    ratio := (b.remoteTokens - b.pendingDelta) / b.capacity
    // clamp to [0,1]
    if ratio < 0 { return 0 }
    if ratio > 1 { return 1 }
    return ratio
}
```

Then in the flush closure (which only exists for `local_cache: true` rules — so the gauge
automatically scopes itself to exactly those rules, which is what we want):

```go
snapshot := manager.Snapshot()
var sum float64
for _, tb := range snapshot {
    sum += tb.FillRatio()
    // ... existing Redis-sync work ...
}
if len(snapshot) > 0 {
    rec.SetTokenBucketFillRatio(rule.Name, sum/float64(len(snapshot))) // mean
} else {
    rec.ClearTokenBucketFillRatio(rule.Name) // no buckets → remove series (Grafana shows "no data")
}
```

The `Clear` (which calls `DeleteLabelValues`) matters: if all buckets go idle and you *kept*
setting `0`, Grafana would draw a solid line at "empty," which is wrong. Deleting the series makes
Grafana correctly show a gap. This is the honest way to represent "I have no data" vs. "the value
is zero."

**Why the example config needs a change:** none of the original four rules in
`config.example.yaml` use `local_cache: true`, so without adding one, this gauge (and its
dashboard panel) sits empty on a fresh `docker-compose up`. The plan adds a 5th rule — a
`token_bucket` rule with `local_cache: true` — keeping the original four as pure-Redis examples.

---

## 10. Wiring it in `main.go` + the second listener

```go
reg := prometheus.NewRegistry()
m, err := metrics.New(reg)          // build observability FIRST
if err != nil {
    return fmt.Errorf("init metrics: %w", err)
}
// ...then inject m into everything:
redisStore := store.NewRedisStore(redisClient, /* scripts... */, m)
engine, err := rules.Compile(cfg, redisStore, m)
mux.Handle("/v1/check", api.NewCheckHandler(holder, m))

// SEPARATE listener for metrics:
metricsMux := http.NewServeMux()
metricsMux.Handle("/metrics", m.Handler())
metricsServer := &http.Server{Addr: opts.metricsAddr, Handler: metricsMux}
```

Why a **separate** `http.Server` on `:9091` instead of adding `/metrics` to the main `:8080` mux?
So you can firewall internal metrics away from public client traffic (`/metrics` leaks rule names
and traffic volumes), and so the two have independent lifecycles.

### Bind-failure policy: fatal at startup, isolated at runtime

```go
metricsErr := make(chan error, 1)
go func() { metricsErr <- metricsServer.ListenAndServe() }()

select {
case err := <-serveErr:      // main :8080 server
    // ...
case err := <-metricsErr:    // :9091 metrics server
    if err != nil && !errors.Is(err, http.ErrServerClosed) {
        return fmt.Errorf("serve metrics on %q: %w", opts.metricsAddr, err) // ← fatal at startup
    }
case <-ctx.Done():
    // graceful shutdown of BOTH servers
}
```

A bind failure (port taken) surfaces from `ListenAndServe` immediately at startup → we return an
error → process exits non-zero. This follows the "explicit failure over silent misbehavior"
principle: a service that boots green but has no metrics is lying about its health. The two
servers stay **independent** — this only makes the **startup** bind fatal; a metrics problem at
runtime can't crash `:8080`. When `-metrics-addr` is empty, the listener is skipped entirely (no
error) — that's the one supported way to disable metrics.

### Config-reload counter

Slots into the *existing* `onChange` closure — one line before each failure `return`, one after
success:

```go
if err := config.Validate(newCfg); err != nil {
    m.IncConfigReload(false)   // ← result="failure"
    fmt.Fprintf(stderr, "limigo: config reload failed: %v\n", err)
    return
}
// ... on success, after holder.Store(newEngine):
m.IncConfigReload(true)        // ← result="success"
```

This gives you a panel that catches "someone pushed a broken config, the reload silently no-op'd,
and they think their new rule is live" — a real and nasty class of production confusion. The
`FlushLocalCaches` failure elsewhere in that closure is not a reload failure and does not touch
this counter.

---

## 11. The Docker glue (how the three programs find each other)

`docker-compose.yml` runs four containers on one virtual network where **containers reach each
other by service name** (Docker provides DNS). That's the whole trick:

- **Prometheus** finds your app because `prometheus/prometheus.yml` says:
  ```yaml
  scrape_configs:
    - job_name: limigo
      scrape_interval: 5s
      static_configs:
        - targets: ["limigo:9091"]   # "limigo" = the compose service name
  ```
- **Grafana** finds Prometheus via a *provisioning* file
  (`grafana/provisioning/datasources/prometheus.yml`) pointing at `http://prometheus:9090`.
  "Provisioning" = config baked in at startup so the datasource and dashboard exist automatically
  — no clicking through the Grafana UI after every `up`.
- **Your app** finds Redis via `REDIS_ADDR=redis:6379`.

The `grafana/dashboard.json` is just an exported dashboard definition (6 panels, each holding a
PromQL query — see §4 and the panel list below). Dropping it in the provisioned folder means it's
there the moment Grafana boots.

The `Dockerfile` is a standard **multi-stage build**: stage 1 uses `golang:1.25` to compile a
static binary; stage 2 copies *just the binary* + `config.example.yaml` into a tiny base image
(the Lua scripts are compiled in via `go:embed`, so they need no copying). Small, no Go toolchain
in the shipped image.

### The 6 dashboard panels

1. **Request rate by outcome** — `sum by (result) (rate(limigo_requests_total[1m]))`
2. **Fail-closed rate** — `sum(rate(limigo_requests_total{result="error"}[5m]))`
3. **Redis round-trip latency percentiles** —
   `histogram_quantile(0.50/0.95/0.99, sum(rate(limigo_redis_latency_seconds_bucket[5m])) by (le))`
4. **Lua execution time percentiles** — same shape against `limigo_lua_execution_seconds_bucket`,
   overlaid with panel 3 so network-vs-script cost is visually obvious
5. **Token bucket fill ratio per rule** — `limigo_token_bucket_fill_ratio`, one line per `rule`
6. **Config reload counter** — `increase(limigo_config_reload_total[1h])` by `result`

---

## 12. How we test it

`client_golang` ships a `testutil` package for exactly this:

- **Counters/gauges** → `testutil.ToFloat64(collector)` reads the single current value. Great for
  "did `RecordRequest("free","denied")` increment the right series and nothing else?"
- **Histograms** → `ToFloat64` **panics** on them (a histogram isn't one number). Use
  `testutil.CollectAndCompare(collector, expectedText)` against a golden text blob asserting
  `_count`/`_sum`.
- **Cardinality guard** → `testutil.GatherAndCount(reg)` after N requests across M rules, assert
  the series count stays within a bound. This is the test that would scream if someone later adds
  a high-cardinality label — cheap insurance against the classic Prometheus meltdown.

Because each test builds its own `prometheus.NewRegistry()`, they're fully isolated — no shared
global state, no ordering dependencies.

---

## 13. Decisions log

Design questions resolved before implementation, with the reasoning:

| # | Decision | Reasoning |
|---|---|---|
| 1 | `Metrics` struct with semantic recorder methods, injected via narrow per-package interfaces | Matches existing `store.go` DI pattern; avoids scattering label logic across call sites |
| 2 | Split `redis_latency_seconds` (client round trip) vs `lua_execution_seconds` (server-measured via `redis.call('TIME')`) | Client-side timing can't separate network from script cost; faking two identical numbers would be dishonest instrumentation |
| 3 | `token_bucket_fill_ratio` = mean across active local-cache buckets, sampled on the existing 10ms flush tick; omitted entirely (not zero) for non-local-cache rules | A gauge can only hold one value per label; per-key state only exists for local-cache rules; a missing series reads correctly in Grafana as "no data," a fake 0 reads as "empty" |
| 4 | `requests_total` uses a 4-way `result` label: `allowed`/`denied`/`unmatched`/`error` | `error` (store broken, fail-closed) and `denied` (working as designed) are operationally opposite and must be distinguishable |
| 5 | OpenTelemetry tracing explicitly out of scope | Separate SDK, separate container, separate instrumentation — kept as a clean follow-up |
| 6 | `/metrics` served on a dedicated `-metrics-addr` listener (default `:9091`), not the main API mux | Operational data shouldn't share a network path/port with public client traffic |
| 7 | Custom histogram buckets (50µs–250ms) instead of Prometheus defaults | Default buckets start at 5ms; sub-millisecond Redis/Lua ops would all collapse into one bucket |
| 8 | Fully self-contained `docker-compose up`: app + Redis + Prometheus + Grafana, dashboard pre-provisioned | No manual UI clicking required to see the system working |
| 9 | Panel list matches the metrics/labels actually implemented, not just CLAUDE.md's original bare list | CLAUDE.md's metric list is a floor, not an exhaustive label spec |
| 10 | Histogram tests use `testutil.CollectAndCompare`, not `ToFloat64` | `ToFloat64` panics on histograms; only works for Counter/Gauge/Untyped |
| 11 | Metrics collection always-on (no toggle in business logic); only the listener itself can be disabled via empty `-metrics-addr` | A toggle would force nil-checks throughout business logic — a reliability hazard for a feature with near-zero cost |
| 12 | `config_reload_total` gets a `result` label (`success`/`failure`), four increment sites in the existing `onChange` closure | Silent hot-reload failures are a real, nasty production confusion class |
| 13 | `algorithm` label (6 values) added to both latency histograms, even though CLAUDE.md's spec didn't list labels for them | Different Lua scripts have very different cost profiles; collapsing them hides which algorithm caused a latency regression |
| 14 | Two stale untracked backup files in `internal/api/` deleted as a separate first commit | Confirmed byte-for-byte duplicates, referenced nowhere; kept out of the metrics diff per "minimal changes" |
| 15 | Add a 5th `local_cache: true` token-bucket rule to `config.example.yaml` | Without it, the fill-ratio gauge and its dashboard panel are empty on a default `docker-compose up`; keeps the original four rules as pure-Redis examples |
| 16 | `/metrics` bind failure is **fatal at startup**, but the two HTTP listeners remain fully independent so a runtime metrics fault can't take down `:8080` | A running-but-unobservable service is silent misbehavior; the failure mode is a deterministic local misconfiguration, the textbook fail-fast case |
