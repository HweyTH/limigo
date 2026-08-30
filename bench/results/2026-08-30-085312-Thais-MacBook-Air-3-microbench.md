# Go microbenchmarks — 2026-08-30-085312

## Hardware

- Host: `Darwin Thais-MacBook-Air-3.local 25.3.0 Darwin Kernel Version 25.3.0: Wed Jan 28 20:54:22 PST 2026; root:xnu-12377.81.4~5/RELEASE_ARM64_T8112 arm64`
- CPU: `Apple M2` (8 logical cores)
- Go: `go version go1.27.0 darwin/arm64`
- Docker: `27.5.1`

## Command

```
bench/run-microbench.sh
```

## Method

Every benchmark below was run **10 times** (`-count=10`) at
`-benchtime=1s` and reduced with
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat), whose
documentation asks for at least 10 runs before it will report a median and a
confidence interval. The `±` column is that interval, not a standard
deviation: it is what says whether a difference between two rows is real or is
the laptop's scheduler and thermal state. Single-run benchmark output is not
published here for that reason.

No baseline/experiment comparison is run: these are absolute cost tables for
one commit, so benchstat prints medians with intervals rather than a delta and
a p-value. Comparing two commits is `benchstat old.txt new.txt` over the raw
files kept alongside this one.

## Layer 1 — pure algorithm cost (`internal/limiter`)

No store, no HTTP, no container. Serial rows are uncontended per-operation
cost; parallel rows spread concurrent callers across 1000 keys through the
algorithm's manager, which is what exposes the per-key routing lock.

```
goos: darwin
goarch: arm64
pkg: github.com/hweyth/limigo/internal/limiter
cpu: Apple M2
                               │ pure-algorithm │
                               │     sec/op     │
FixedWindow_Serial-8                28.56n ± 1%
FixedWindow_Parallel-8              10.62n ± 2%
SlidingWindow_Serial-8              48.86n ± 1%
SlidingWindow_Parallel-8            91.31n ± 1%
TokenBucket_Serial-8                77.45n ± 1%
TokenBucket_Parallel-8              89.93n ± 2%
LeakyBucket_Serial-8                57.69n ± 1%
LeakyBucket_Parallel-8              91.88n ± 1%
BatchingFixedWindow_Serial-8        10.71n ± 1%
BatchingFixedWindow_Parallel-8      88.00n ± 2%
BatchingTokenBucket_Serial-8        10.70n ± 0%
BatchingTokenBucket_Parallel-8      88.98n ± 2%
geomean                             43.41n

                               │ pure-algorithm │
                               │      B/op      │
FixedWindow_Serial-8               0.000 ± 0%
FixedWindow_Parallel-8             0.000 ± 0%
SlidingWindow_Serial-8             0.000 ± 0%
SlidingWindow_Parallel-8           4.000 ± 0%
TokenBucket_Serial-8               0.000 ± 0%
TokenBucket_Parallel-8             2.000 ± 0%
LeakyBucket_Serial-8               0.000 ± 0%
LeakyBucket_Parallel-8             2.000 ± 0%
BatchingFixedWindow_Serial-8       0.000 ± 0%
BatchingFixedWindow_Parallel-8     2.000 ± 0%
BatchingTokenBucket_Serial-8       0.000 ± 0%
BatchingTokenBucket_Parallel-8     2.000 ± 0%
geomean                                       ¹
¹ summaries must be >0 to compute geomean

                               │ pure-algorithm │
                               │   allocs/op    │
FixedWindow_Serial-8               0.000 ± 0%
FixedWindow_Parallel-8             0.000 ± 0%
SlidingWindow_Serial-8             0.000 ± 0%
SlidingWindow_Parallel-8           0.000 ± 0%
TokenBucket_Serial-8               0.000 ± 0%
TokenBucket_Parallel-8             0.000 ± 0%
LeakyBucket_Serial-8               0.000 ± 0%
LeakyBucket_Parallel-8             0.000 ± 0%
BatchingFixedWindow_Serial-8       0.000 ± 0%
BatchingFixedWindow_Parallel-8     0.000 ± 0%
BatchingTokenBucket_Serial-8       0.000 ± 0%
BatchingTokenBucket_Parallel-8     0.000 ± 0%
geomean                                       ¹
¹ summaries must be >0 to compute geomean
```

## Layer 2 — full rule evaluation against real Redis (`internal/rules`)

Rule match + Lua execution + round trip, against a real Redis container
(A fake would measure Go function-call overhead and report it as
Lua execution cost). The uncached/cached pair is the same
`burst-tier-token-bucket` / `cached-tier-token-bucket` A/B used elsewhere:
identical capacity and refill rate, differing only in `local_cache`.

```
goos: darwin
goarch: arm64
pkg: github.com/hweyth/limigo/internal/rules
cpu: Apple M2
                       │ engine+redis │
                       │    sec/op    │
EngineCheck_Uncached-8    122.9µ ± 5%
EngineCheck_Cached-8      25.18n ± 0%
geomean                   1.759µ

                       │ engine+redis │
                       │     B/op     │
EngineCheck_Uncached-8   528.0 ± 0%
EngineCheck_Cached-8     0.000 ± 0%
geomean                             ¹
¹ summaries must be >0 to compute geomean

                       │ engine+redis │
                       │  allocs/op   │
EngineCheck_Uncached-8   19.00 ± 0%
EngineCheck_Cached-8     0.000 ± 0%
geomean                             ¹
¹ summaries must be >0 to compute geomean
```

Raw `go test -bench` output (the input benchstat parsed) is kept under
`bench/results/raw/2026-08-30-085312-microbench/` (gitignored — regenerate by rerunning
this script).
