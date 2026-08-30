# Throughput measurement suite — 2026-08-30-085730

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

## Measurement model — why throughput and latency come from different runs

**No table in this file reports throughput and latency from the same run.**

Throughput rows are closed-loop: `-rate=0 -max-workers=200`, meaning
each of 200 workers sends a request, waits for the response, then sends
the next. That measures max sustainable req/s correctly, and measures latency
badly. If the service stalls, every worker is parked waiting, so the requests
that were due to be sent during the stall are never sent and never timed — the
worst moments delete their own evidence, and the reported p99 improves because
things got worse. Gil Tene named this **coordinated omission**. vegeta is
normally resistant to it (it timestamps at actual send time and grows its worker
pool to catch back up), but a fixed `-max-workers` cap removes the pool growth
that resistance depends on.

Latency rows are therefore open-model: a **fixed arrival rate** with **no
`-max-workers` cap**, so vegeta keeps its schedule through a stall instead of
coordinating with it. The rate is 10761 req/s — 70% of the
through-Traefik ceiling measured in this same run — leaving the generator
headroom to catch up. Each latency table prints the offered rate beside the
attained rate: if they diverge, the generator failed to keep schedule and the
percentiles beside them describe a saturated generator, not the service.

## Control rows (ADR-0004)

GET /healthz, no rule logic, static 200 (CONTEXT.md, control run). Every table
below opens with these and reports limiter throughput as a cost relative to row 2
(through-Traefik, in-network) — the ceiling the algorithm and node axes actually
run against, since they too go through Traefik.

| row | requests | req/s | success |
|---|---|---|---|
| 1. direct-to-replica, in-network | 537479 | 17916 | 100.00% |
| 2. through-Traefik, in-network (ceiling used below) | 461232 | 15374 | 100.00% |
| 3. host-origin (through Traefik) | 952633 | 31749 | 100.00% |

The same control path, measured open-model for its latency:

| row | offered req/s | attained req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |
|---|---|---|---|---|---|---|---|
| through-Traefik, in-network | 10761 | 10761 | 100.00% | 0.19 | 0.47 | 1.20 | 13.16 |

### Redis-side bound — `redis-benchmark evalsha`

The control rows above bound the **transport**: generator, Traefik and a static
200, with no rule logic. This bounds the **other end** — what Redis alone can do
with Limigo's own `token_bucket.lua`, driven by Redis's own benchmark tool with
no Go, no HTTP and no JSON in the path. Every uncached allow-path request must
wait for exactly this script, so no uncached row in this file can exceed it.

| bound | req/s | p99 (ms) |
|---|---|---|
| `redis-benchmark evalsha` (`token_bucket.lua`, SHA `32cfdd87d44393c53903631278c4270c9ed95e46`) | 89126.56 | 1.039 |

Run as `redis-benchmark -h redis -n 100000 -c 50 -P 1 -r 10000 evalsha <sha> 1
limigo:bench:__rand_int__ 1000000 1000000`, in-network and pinned to the same
cores as the vegeta generator. The script is `SCRIPT LOAD`ed from the same file
`internal/store/lua` embeds, so the SHA benchmarked is the SHA Limigo runs.
`-r 10000` expands `__rand_int__` over a 10k-value keyspace to match the
cardinality of the algorithm and node axes; the two ARGV values are capacity and
refill rate, set high enough that the script stays on its allow path. `-P 1`
leaves pipelining off because Limigo issues one `EVALSHA` per request and does
not pipeline.

Two caveats this row does not hide: `redis-benchmark` drives its own 50
connections rather than replaying Limigo's concurrency, and it is co-resident
with Redis in the same Docker VM as everything else here. It is an upper bound
on the Redis leg under this tool's load, not a claim about Redis in general.

## Algorithm axis — allow path, uncached, 1 replica

POST /v1/check through Traefik, in-network, rules from `bench/config.loadtest.yaml`
(headroom limits — allow path stays hot). 1 key measures single-bucket contention;
10k keys measures map growth and Redis keyspace behaviour. Never blended.

| row | requests | req/s | success | cost vs ceiling |
|---|---|---|---|---|
| fixed_window, 1 key(s) | 381611 | 12720 | 100.00% | 82.7% of ceiling (cost ~17.3%) |
| fixed_window, 10000 key(s) | 406017 | 13534 | 100.00% | 88.0% of ceiling (cost ~12.0%) |
| sliding_window, 1 key(s) | 400656 | 13355 | 100.00% | 86.9% of ceiling (cost ~13.1%) |
| sliding_window, 10000 key(s) | 409422 | 13647 | 100.00% | 88.8% of ceiling (cost ~11.2%) |
| token_bucket, 1 key(s) | 405713 | 13524 | 100.00% | 88.0% of ceiling (cost ~12.0%) |
| token_bucket, 10000 key(s) | 414332 | 13811 | 100.00% | 89.8% of ceiling (cost ~10.2%) |
| leaky_bucket, 1 key(s) | 372973 | 12432 | 100.00% | 80.9% of ceiling (cost ~19.1%) |
| leaky_bucket, 10000 key(s) | 377194 | 12573 | 100.00% | 81.8% of ceiling (cost ~18.2%) |

### Algorithm axis — latency, open-model

The same four algorithms at 10k keys, re-run open-model at 10761 req/s
(70% of the measured ceiling, uncapped worker pool) because the percentiles from
the closed-loop table above would be coordinated-omission-contaminated. 10k keys
only: that is the cardinality representing realistic keyspace behaviour rather
than single-bucket contention.

| row | offered req/s | attained req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |
|---|---|---|---|---|---|---|---|
| fixed_window, 10000 key(s) | 10761 | 10761 | 100.00% | 0.30 | 0.77 | 1.92 | 23.79 |
| sliding_window, 10000 key(s) | 10761 | 10761 | 100.00% | 0.31 | 0.84 | 2.23 | 23.48 |
| token_bucket, 10000 key(s) | 10761 | 10761 | 100.00% | 0.33 | 0.84 | 1.86 | 23.64 |
| leaky_bucket, 10000 key(s) | 10761 | 10761 | 100.00% | 0.43 | 1.57 | 4.94 | 27.86 |

## Denial path — labelled separately, never blended with the allow-path rows above

Same shape of request, matched instead by `load-test-deny` (limit 1 per 60s), so
every request past the first is a denial. Denials are cheaper (CONTEXT.md,
allow path vs denial path); this row exists to show that, not to represent
achievable allow-path throughput.

| row | requests | req/s | success | cost vs ceiling |
|---|---|---|---|---|
| fixed_window (limit 1/60s), 1 key | 362966 | 12099 | 0.00% | 78.7% of ceiling (cost ~21.3%) |

The 0.00% success column is the expected result, not a failure: vegeta counts a
429 as unsuccessful, and by design every request here past the first is a 429.

This row's req/s was recomputed from this same run's raw capture after the
harness was corrected to report vegeta's `.rate` (requests served per second,
any verdict) rather than `.throughput` (successful requests per second, which
counts a 429 as unsuccessful and so reported 0 req/s for a run that served
twelve thousand denials a second). Every other row in this file is unaffected —
they are all 100% success, where the two fields are identical.

## Node axis — scaling curve (ADR-0002)

token_bucket, 10k keys, allow path, through Traefik. The claim under test is
approximately linear throughput growth with node count — not any particular
absolute number. Cost is reported against the single-replica through-Traefik
ceiling from the control rows above: that ceiling is a property of the
generator/Traefik/network path, not of how many limigo replicas sit behind it,
so it does not need re-measuring per node count — a row exceeding 100% here
would itself be a finding (Traefik load-balancing outrunning a single-replica
baseline), not an error.

| row | requests | req/s | success | cost vs ceiling |
|---|---|---|---|---|
| 1 replica(s) | 371012 | 12367 | 100.00% | 80.4% of ceiling (cost ~19.6%) |
| 2 replica(s) | 394415 | 13147 | 100.00% | 85.5% of ceiling (cost ~14.5%) |
| 3 replica(s) | 382978 | 12765 | 100.00% | 83.0% of ceiling (cost ~17.0%) |

## Cached vs uncached — controlled comparison (config.example.yaml pair)

`burst-tier-token-bucket` (capacity 1000, rate 200, no cache) vs
`cached-tier-token-bucket` (identical parameters, `local_cache: true`).
Both held to a fixed **150 req/s** — under the shared 200/s refill rate — so
the allow path stays hot for both arms instead of collapsing into the denial
path once the 1000-token burst capacity drains. At this rate the comparison is
latency (does the local cache avoid a Redis round-trip), not max throughput —
measuring throughput at these low, realistic limits would mostly measure how
fast Limigo can say no once capacity is exhausted, which is ticket 08's
overshoot harness, not this one's.

Both arms are open-model — fixed arrival rate, uncapped worker pool — so these
percentiles carry the same coordinated-omission guarantee as the latency tables
above. Both are offered the same 150 req/s, so the attained-rate column is a
check that neither arm fell behind, not a result; the comparison is the
percentiles to its right.

| row | offered req/s | attained req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |
|---|---|---|---|---|---|---|---|
| uncached (burst-tier-token-bucket) | 150 | 150 | 100.00% | 1.21 | 2.12 | 3.40 | 12.65 |
| cached (cached-tier-token-bucket, local_cache: true) | 150 | 150 | 100.00% | 1.00 | 1.34 | 1.92 | 10.38 |

Raw vegeta output (binary attack results, generated target files, and per-run
target JSONL) is kept under `bench/results/raw/2026-08-30-085730-throughput/` (gitignored —
regenerate by rerunning this script rather than diffing binary blobs).
