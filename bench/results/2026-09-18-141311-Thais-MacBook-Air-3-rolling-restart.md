# Rolling restart under load — 2026-09-18-141311

## Hardware

- Host: `Darwin Thais-MacBook-Air-3.local 25.3.0 Darwin Kernel Version 25.3.0: Wed Jan 28 20:54:22 PST 2026; root:xnu-12377.81.4~5/RELEASE_ARM64_T8112 arm64`
- CPU: `Apple M2` (8 logical cores)
- Docker: `27.5.1`

## Command

```
bench/run-rolling-restart.sh --duration 130
```

## Method

One key, **100 req/s for 130s** against `burst-tier-token-bucket` (refill
200/s, so every request is admitted while the stack is healthy), **3
replicas** behind Traefik. From **t+15s**, each replica in turn is
restarted with `docker restart -t 20` (SIGTERM; the replica fails `/readyz`, keeps
serving for its 6s drain delay, then drains in flight and exits; Docker starts
it again) and the next restart waits until the previous replica is healthy and
has had one Traefik check interval to rejoin the pool. Responses are bucketed
per second from vegeta's own timestamps; restart stamps are from the host.

**Dropped** = any 5xx (a replica's 503, or Traefik's 502/504 when it had
nowhere to route) plus any request with no HTTP response at all.

## Result

**0 of 13000 requests dropped** across 3 rolling restarts
(5xx: 0, no response: 0; 429: 0, which must be 0 for the
run to be valid).

vegeta status-code histogram: `200=13000`

### Restart timeline (generator seconds; t+0 is the first request)

| replica | SIGTERM sent | back up | healthy and re-pooled |
|---|---|---|---|
| `limigo-limigo-1` | t+17s | t+23s | t+35s |
| `limigo-limigo-2` | t+35s | t+42s | t+54s |
| `limigo-limigo-3` | t+54s | t+64s | t+76s |

Host stamped the generator's launch at t+0s, so host and
container clocks agree to within a second.

### Per-second timeline, collapsed into runs of identical shape

| seconds | 200 allowed | 429 denied | 5xx | other (no HTTP response) |
|---|---|---|---|---|
| 0–130 | 13000 | 0 | 0 | 0 |
| **total** | **13000** | **0** | **0** | **0** |

Raw vegeta output, decoded JSON lines, and per-second buckets are kept under
`bench/results/raw/2026-09-18-141311-rolling-restart/` (gitignored — regenerate by rerunning
this script rather than diffing binary blobs).
