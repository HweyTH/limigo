# Throughput measurement suite — 2026-08-23-230659

## Hardware

- Host: `Darwin Thais-MacBook-Air-3.local 25.3.0 Darwin Kernel Version 25.3.0: Wed Jan 28 20:54:22 PST 2026; root:xnu-12377.81.4~5/RELEASE_ARM64_T8112 arm64`
- CPU: `Apple M2` (8 logical cores)
- Docker: `27.5.1`

## Command

```
bench/run-throughput.sh
```

CPU pinning: limigo replica(s) on cores 0-3; in-network generator on
cores 4-7. The control rows' host-origin leg is **not** CPU-pinned —
macOS has no per-process CPU affinity API (see bench/run.sh).

## Control rows (ADR-0004)

GET /healthz, no rule logic, static 200 (CONTEXT.md, control run). Every table
below opens with these and reports limiter throughput as a cost relative to row 2
(through-Traefik, in-network) — the ceiling the algorithm and node axes actually
run against, since they too go through Traefik.

| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |
|---|---|---|---|---|---|---|---|
| 1. direct-to-replica, in-network | 555523 | 18517 | 100.00% | 0.09 | 0.32 | 1.64 | 29.81 |
| 2. through-Traefik, in-network (ceiling used below) | 475576 | 15852 | 100.00% | 0.27 | 0.64 | 1.92 | 25.35 |
| 3. host-origin (through Traefik) | 945248 | 31503 | 100.00% | 5.62 | 12.19 | 17.44 | 77.83 |

## Algorithm axis — allow path, uncached, 1 replica

POST /v1/check through Traefik, in-network, rules from `bench/config.loadtest.yaml`
(headroom limits — allow path stays hot). 1 key measures single-bucket contention;
10k keys measures map growth and Redis keyspace behaviour. Never blended.

| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |
|---|---|---|---|---|---|---|---|---|
| fixed_window, 1 key(s) | 406915 | 13564 | 100.00% | 0.43 | 0.99 | 2.27 | 35.09 | 85.6% of ceiling (cost ~14.4%) |
| fixed_window, 10000 key(s) | 411772 | 13725 | 100.00% | 0.43 | 0.90 | 1.67 | 33.50 | 86.6% of ceiling (cost ~13.4%) |
| sliding_window, 1 key(s) | 394188 | 13139 | 100.00% | 0.47 | 1.09 | 2.29 | 16.56 | 82.9% of ceiling (cost ~17.1%) |
| sliding_window, 10000 key(s) | 402278 | 13409 | 100.00% | 0.46 | 0.94 | 1.69 | 14.66 | 84.6% of ceiling (cost ~15.4%) |
| token_bucket, 1 key(s) | 402501 | 13417 | 100.00% | 0.46 | 1.02 | 2.09 | 14.55 | 84.6% of ceiling (cost ~15.4%) |
| token_bucket, 10000 key(s) | 404816 | 13494 | 100.00% | 0.46 | 0.91 | 1.68 | 34.40 | 85.1% of ceiling (cost ~14.9%) |
| leaky_bucket, 1 key(s) | 397811 | 13260 | 100.00% | 0.47 | 1.08 | 2.28 | 13.86 | 83.6% of ceiling (cost ~16.4%) |
| leaky_bucket, 10000 key(s) | 406826 | 13561 | 100.00% | 0.46 | 0.98 | 1.98 | 14.10 | 85.5% of ceiling (cost ~14.5%) |

## Denial path — labelled separately, never blended with the allow-path rows above

Same shape of request, matched instead by `load-test-deny` (limit 1 per 60s), so
every request past the first is a denial. Denials are cheaper (CONTEXT.md,
allow path vs denial path); this row exists to show that, not to represent
achievable allow-path throughput.

| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |
|---|---|---|---|---|---|---|---|---|
| fixed_window (limit 1/60s), 1 key | 403341 | 0 | 0.00% | 0.43 | 1.03 | 2.25 | 12.36 | 0.0% of ceiling (cost ~100.0%) |

## Node axis — scaling curve (ADR-0002)

token_bucket, 10k keys, allow path, through Traefik. The claim under test is
approximately linear throughput growth with node count — not any particular
absolute number. Cost is reported against the single-replica through-Traefik
ceiling from the control rows above: that ceiling is a property of the
generator/Traefik/network path, not of how many limigo replicas sit behind it,
so it does not need re-measuring per node count — a row exceeding 100% here
would itself be a finding (Traefik load-balancing outrunning a single-replica
baseline), not an error.

| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |
|---|---|---|---|---|---|---|---|---|
| 1 replica(s) | 402008 | 13400 | 100.00% | 0.46 | 0.95 | 1.73 | 51.46 | 84.5% of ceiling (cost ~15.5%) |
| 2 replica(s) | 396587 | 13219 | 100.00% | 0.46 | 0.93 | 1.60 | 47.19 | 83.4% of ceiling (cost ~16.6%) |
| 3 replica(s) | 385606 | 12853 | 100.00% | 0.46 | 0.93 | 1.56 | 46.50 | 81.1% of ceiling (cost ~18.9%) |

## Cached vs uncached — controlled comparison (config.example.yaml pair)

`burst-tier-token-bucket` (capacity 1000, rate 200, no cache) vs
`cached-tier-token-bucket` (identical parameters, `local_cache: true`).
Both held to a fixed **150 req/s** — under the shared 200/s refill rate — so
the allow path stays hot for both arms instead of collapsing into the denial
path once the 1000-token burst capacity drains. At this rate the comparison is
latency (does the local cache avoid a Redis round-trip), not max throughput —
measuring throughput at these low, realistic limits would mostly measure how
fast Limigo can say no once capacity is exhausted, which is what
bench/run-overshoot.sh measures, not this harness.

Both rows are throttled to the same 150 req/s, so their near-identical, near-0%
cost-vs-ceiling figures are expected and not the point of this table — the
column is kept for consistency with every other table here (ADR-0004: never a
bare absolute). The comparison that matters is the latency columns to its left.

| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |
|---|---|---|---|---|---|---|---|---|
| uncached (burst-tier-token-bucket) | 4500 | 150 | 100.00% | 1.30 | 1.93 | 2.79 | 14.54 | 0.9% of ceiling (cost ~99.1%) |
| cached (cached-tier-token-bucket, local_cache: true) | 4500 | 150 | 100.00% | 1.00 | 1.37 | 2.00 | 6.60 | 0.9% of ceiling (cost ~99.1%) |

Raw vegeta output (binary attack results, generated target files, and per-run
target JSONL) is kept under `bench/results/raw/2026-08-23-230659-throughput/` (gitignored —
regenerate by rerunning this script rather than diffing binary blobs).
