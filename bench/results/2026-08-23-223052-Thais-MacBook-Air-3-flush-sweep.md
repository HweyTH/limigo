# Flush-interval accuracy/latency sweep — 2026-08-23-223052

## Hardware

- Host: `Darwin Thais-MacBook-Air-3.local 25.3.0 Darwin Kernel Version 25.3.0: Wed Jan 28 20:54:22 PST 2026; root:xnu-12377.81.4~5/RELEASE_ARM64_T8112 arm64`
- CPU: `Apple M2` (8 logical cores)
- Docker: `27.5.1`

## Command

```
bench/run-flush-sweep.sh
```

## Method

`config.example.yaml`'s `cached-tier-token-bucket` (capacity 1000, rate
200/s, `local_cache: true`) — the same rule ticket 08 uses. Only the
cached arm is swept; the uncached arm doesn't read the flush interval at all.

At every point, exactly **6000 requests** are fired at a single key over
**3s** (rate 2000/1s), against a fresh stack (empty Redis,
no state carried over from the previous point). Expected ceiling = capacity +
rate * seconds = 1000 + 200 * 3 = **1600**
— the most a correct, atomic token bucket can admit over the window regardless
of node count or flush interval. Overshoot = (admitted - 1600) /
1600, as a percentage.

This offered load exceeds the ceiling on purpose (same as ticket 08), so most
requests are denied once the bucket empties. Denial-path latency is cheap and
unrelated to the flush interval (CONTEXT.md), so latency percentiles below are
computed from admitted (allow-path) responses only, decoded from the raw vegeta
results rather than read off vegeta's own blended aggregate — the admitted-sample
count is reported alongside so a thin sample is visible, not hidden.

Node count is held fixed within each sweep below; the flush interval is the only
variable. The first sweep (node=3) is the primary result; further
sweeps at other node counts show how the curve shifts with node count.

## Sweep — node=3

| flush interval | sent | admitted | denied | other | ceiling | overshoot | admitted samples | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1ms | 5998 | 1599 | 4399 | 0 | 1600 | -0.06% | 1599 | 0.328833 | 0.596875 | 1.097041 | 2.857042 |
| 10ms | 5999 | 1599 | 4400 | 0 | 1600 | -0.06% | 1599 | 0.336375 | 0.585583 | 0.963125 | 2.702459 |
| 100ms | 6000 | 1617 | 4383 | 0 | 1600 | 1.06% | 1617 | 0.334917 | 0.646791 | 1.155958 | 4.493583 |
| 1000ms | 5999 | 2365 | 3634 | 0 | 1600 | 47.81% | 2365 | 0.315584 | 0.715417 | 1.711125 | 4.15525 |

## Sweep — node=1

| flush interval | sent | admitted | denied | other | ceiling | overshoot | admitted samples | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1ms | 5999 | 1599 | 4400 | 0 | 1600 | -0.06% | 1599 | 0.320917 | 0.609292 | 1.621667 | 5.518375 |
| 10ms | 6000 | 1598 | 4402 | 0 | 1600 | -0.12% | 1598 | 0.349042 | 0.650083 | 1.444625 | 2.720833 |
| 100ms | 5998 | 1579 | 4419 | 0 | 1600 | -1.31% | 1579 | 0.337875 | 0.623333 | 0.951125 | 2.213791 |
| 1000ms | 5997 | 1361 | 4636 | 0 | 1600 | -14.94% | 1361 | 0.320167 | 0.66625 | 1.648875 | 4.571167 |

## Direction and slope

Expectation (ADR-0005, CONTEXT.md): a shorter flush interval reconciles the
local cache with Redis more often, so it should mean tighter accuracy (smaller
|overshoot|) at the cost of more frequent Redis round-trips — which should show
up as *lower* admitted-path latency being harder to sustain, or as latency rising
at the short end of the sweep if the flush goroutine starts contending with the
request path. Observed, comparing the shortest and longest interval in each sweep:

- node=3: |overshoot| widens from 0.06% at 1ms to 47.81% at 1000ms (796.8x). admitted-path p99 latency rises from 1.097ms at 1ms to 1.711ms at 1000ms.
- node=1: |overshoot| widens from 0.06% at 1ms to 14.94% at 1000ms (249.0x). admitted-path p99 latency rises from 1.622ms at 1ms to 1.649ms at 1000ms.

No flat or non-monotonic curve detected in any sweep above (heuristic: total
|overshoot| range across the sweep, and consecutive-point ordering — see
script). Read the tables directly for the actual slope; this check only flags
sweeps worth a closer look, it does not replace reading them.

Raw vegeta output, generated target files, and the decoded admitted-latency
samples are kept under `bench/results/raw/2026-08-23-223052-flush-sweep/` (gitignored —
regenerate by rerunning this script rather than diffing binary blobs).
