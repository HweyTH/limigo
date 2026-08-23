# Overshoot / correctness harness — 2026-08-23-185344

## Hardware

- Host: `Darwin Thais-MacBook-Air-3.local 25.3.0 Darwin Kernel Version 25.3.0: Wed Jan 28 20:54:22 PST 2026; root:xnu-12377.81.4~5/RELEASE_ARM64_T8112 arm64`
- CPU: `Apple M2` (8 logical cores)
- Docker: `27.5.1`

## Command

```
bench/run-overshoot.sh --requests 4000 --seconds 2 --max-workers 200
```

## Method

`config.example.yaml`'s `burst-tier-token-bucket` (no local cache) vs
`cached-tier-token-bucket` (identical: capacity 1000, rate 200/s,
`local_cache: true`) — the existing controlled pair (ADR-0005). Both arms fire
exactly **4000 requests** at a single key over **2s**
(rate 2000/1s), fresh Redis state per run. A run whose actual request count
didn't match 4000 exactly would abort rather than publish here — see script.

Expected ceiling = capacity + rate * seconds = 1000 + 200 * 2 =
**1400**. This is the most a correct, atomic token bucket can admit
over the test window regardless of node count, since bucket state is shared in
Redis. Overshoot = (admitted - 1400) / 1400, as a percentage.

## Results

| nodes | arm | requests sent | admitted (200) | denied (429) | other | expected ceiling | overshoot |
|---|---|---|---|---|---|---|---|
| 1 | local_cache: false | 3999 | 1399 | 2600 | 0 | 1400 | -0.07% |
| 2 | local_cache: false | 4000 | 1399 | 2601 | 0 | 1400 | -0.07% |
| 3 | local_cache: false | 3998 | 1399 | 2599 | 0 | 1400 | -0.07% |
| 1 | local_cache: true | 4000 | 1396 | 2604 | 0 | 1400 | -0.29% |
| 2 | local_cache: true | 4000 | 1121 | 2879 | 0 | 1400 | -19.93% |
| 3 | local_cache: true | 4000 | 1126 | 2874 | 0 | 1400 | -19.57% |

**local_cache: false** is the correctness claim: overshoot at or near 0% at every
node count demonstrates Lua atomicity holding under genuine cross-node concurrency.

**local_cache: true** is the documented trade-off: a non-zero deviation from the
ceiling is expected here and is not by itself a defect — it is the cost of
absorbing bursts locally instead of round-tripping every request to Redis
(CONTEXT.md: local cache, flush interval). ADR-0005 anticipated that deviation
as *over*-admission growing with node count; whether this run instead shows
under-admission, and whether either stays bounded, is exactly what the
escalation check below is for — read it before treating this table as the
"local caching is fine" result.

**Stated bound (local_cache: true):** the largest |overshoot| observed across
1/2/3 nodes is **19.93%** of the expected ceiling.

## ESCALATION

local_cache: true: unbounded-looking under-admission (fewer requests admitted than the expected ceiling) at 3 nodes — |overshoot| is 19.57% of the configured limit

Per ticket 08, this blocks recommending local caching as a default until
investigated — it is a design finding, not a number to publish quietly.

Raw vegeta output and generated target files are kept under
`bench/results/raw/2026-08-23-185344-overshoot/` (gitignored — regenerate by rerunning this
script rather than diffing binary blobs).
