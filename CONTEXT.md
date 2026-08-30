# Limigo — Shared Context

Working vocabulary for this codebase. When a word here is used in an issue, a
commit message, an ADR, or a README table, it carries the meaning defined below
and no other. If a term starts doing two jobs, split it rather than overloading it.

See `docs/adr/` for the decisions this vocabulary came out of.

---

## Core domain

**Rule** — one entry in the config file. Pairs a *match* predicate (currently a
header name/value) with an *algorithm* and its parameters. Rules are ordered;
the first match wins.

**Key** — the caller-supplied identity a limit is applied to, sent as `key` in
the `POST /v1/check` body. One key has independent limiter state per rule.

**Decision** — what `Engine.Check` returns: matched or not, allowed or not, and
optionally a retry-after. Distinct from the *HTTP response*, which also encodes
store failure as 503.

**Engine** — the compiled, immutable rule set (`internal/rules`). Rebuilt wholesale
on config reload and swapped behind `EngineHolder`; never mutated in place.

**Node** — one Limigo process. Nodes are stateless and interchangeable; all shared
counter state lives in Redis. "3 nodes" means three replicas of the same image
behind one entry point, not three differently-configured services.

**Algorithm** — one of `fixed_window`, `sliding_window`, `token_bucket`,
`leaky_bucket`. Selected per rule.

---

## Consistency vocabulary

**Local cache** — the per-node, in-process counter state a rule opts into with
`local_cache: true`. Absorbs bursts without a Redis round-trip.

**Flush interval** — how often a node reconciles its local cache with Redis
(`--cache-flush-interval`, default 10ms). The tunable knob on the accuracy/latency
trade-off.

**Overshoot** — the number of requests admitted *above* a rule's configured limit
because local caches had not yet reconciled. Expressed as a percentage of the
configured limit. Expected to be ~0% with `local_cache: false` and non-zero but
bounded with `local_cache: true`, growing with node count and flush interval.
**Overshoot is a measured quantity in this project, not an acknowledged risk** —
see ADR-0005.

**Allow path** — the code path taken when a request is *admitted*. Expensive: it
mutates counter state and, for uncached rules, executes Lua on Redis.

**Denial path** — the path taken when a request is *rejected*. Cheaper, and on
cached rules may not touch Redis at all. Numbers from the two paths are never
blended into one figure; a benchmark that denies most requests is measuring the
cheap path and must be labelled as such.

---

## Measurement vocabulary

**Control run** — a load test against `GET /healthz`, which returns a static 200
and touches no rule logic. Establishes the *ceiling* imposed by the environment
rather than by Limigo. Every results table starts with a control row.

**Ceiling** — the throughput a control run achieves. Limiter results are reported
as a cost *relative to the ceiling* ("28k req/s against a 30k ceiling — rate
limiting costs ~7%"), never as bare absolute numbers.

**In-network** — load generated from a container on the compose network. The
*primary* number: it characterises Limigo rather than the host's virtualisation layer.

**Host-origin** — load generated from the host, crossing the Docker Desktop VM
boundary. The *secondary* number. The gap between in-network and host-origin is
itself a reported finding, not noise to be discarded.

**Cardinality** — how many distinct keys a load test spreads across. Reported at
two points, 1 key and 10k keys, never blended: 1 key measures contention on a
single bucket, 10k measures map growth and Redis keyspace behaviour.

**Scaling curve** — throughput and overshoot measured at 1, 2, and 3 replicas.
Replaces the flat "50k req/s" claim; see ADR-0002.

---

## Standing rules

- No published performance number exists without stated hardware and a
  reproducible command. See ADR-0002.
- No aspirational figure appears next to a measured one.
- Correctness is measured alongside throughput, never assumed from it. See ADR-0005.
