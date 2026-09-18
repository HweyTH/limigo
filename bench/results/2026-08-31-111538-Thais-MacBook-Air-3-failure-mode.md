# Failure mode — Redis outage — 2026-08-31-111538

## Hardware

- Host: `Darwin Thais-MacBook-Air-3.local 25.3.0 Darwin Kernel Version 25.3.0: Wed Jan 28 20:54:22 PST 2026; root:xnu-12377.81.4~5/RELEASE_ARM64_T8112 arm64`
- CPU: `Apple M2` (8 logical cores)
- Docker: `27.5.1`

## Command

```
bench/run-failure-mode.sh
```

## Method

One limigo replica against `config.example.yaml`'s controlled pair:
`burst-tier-token-bucket` (no local cache) and `cached-tier-token-bucket`
(identical — capacity 1000, rate 200/s — except `local_cache: true`).
Both arms are attacked concurrently at **100 req/s** against a single key for
**50s**, open-model with no worker cap. The probe rate is held below the
rules' shared 200/s refill, so with Redis healthy every request is on the
allow path and any non-200 belongs to the outage.

Redis is stopped 10s in (`docker compose stop -t 0 redis` — abrupt, and not
the `kill` the container's `restart: unless-stopped` policy would immediately
undo) and started again 25s later, leaving 15s of recovery in the window.

**Where the outage window comes from.** vegeta timestamps inside the Docker VM
and the outage is triggered from the host, so the two boundary instants are not
taken from the host clock at all. They are read out of the **uncached arm's own
timeline**: every uncached decision needs a live round-trip, so that arm's first
failure is the first probe issued after the store went away and its first
success afterwards is the first probe issued after it came back. Both markers
therefore sit in the same clock as every timestamp subtracted from them, and the
10ms gap between probes at 100 req/s is the floor on every figure below.
The consequence is that the uncached arm cannot measure its own flip time — it
*defines* the zero — so its row reads `marker` rather than a suspiciously round
0ms. What that row does measure is which failure the arm produced and for how
long, which is the part in question.

Host and container clocks were checked to agree within **0s** (busybox has no
sub-second `date`, so a second is as fine as this resolves), which is enough to
confirm the markers fall in the run segment they claim to. The observed outage
was **29.30s** long against the 25s requested; the stop command itself took
3529ms and the start command 3703ms, which is where that difference comes from.

**No control row here, deliberately.** Every other results file in this
directory opens with a `/healthz` run, because it publishes throughput or
latency and those are meaningless without the ceiling the environment imposes
(CONTEXT.md: control run, ceiling). Nothing below is a rate or a percentile —
the figures are response codes, counts, and two transition times — so there is
no ceiling to report them against. What plays the control's role instead is the
uncached arm, which differs from the cached arm in exactly one config field.

**Expected cached survival:** a cached bucket decides against a snapshot of the
authoritative token count and does not simulate refill locally
(`limiter.BatchingTokenBucket`), so with the snapshot sitting at capacity when
the store disappears it can admit `capacity / rate` = 1000 / 100 =
**10.0s** of requests before it must start denying. Hardware-independent,
like the overshoot harness's expected ceiling.

## Results — response behaviour

| arm | baseline 200s | time to first non-200 | first failure code | outage: 200 | outage: 429 | outage: 503 | outage: other |
|---|---|---|---|---|---|---|---|
| local_cache: false | 1469 | marker | 503 | 0 | 0 | 2930 | 0 |
| local_cache: true | 1470 | 9.99s | 429 | 999 | 1931 | 0 | 0 |

## Results — recovery

| arm | back to 200 | post-restart 200s | post-restart non-200s | of those, after the first 200 |
|---|---|---|---|---|
| local_cache: false | marker | 600 | 1 | 1 |
| local_cache: true | 114ms | 589 | 11 | 0 |

The last two columns are not the same number twice. Every non-200 in the
post-restart segment is counted first, then only those that landed *after* the
arm's first success — a denial while an arm is still waiting to recover is
expected, a failure after it has already recovered is not, and collapsing them
into one column would hide whichever is the interesting one.

## Results — `limigo_requests_total` during the outage

Counter increments between a scrape taken as soon as the stop command returned
and one taken as soon as the start command returned, read from limigo's own
`/metrics` rather than through Prometheus (a 5s scrape interval cannot resolve a
window this short, and these counters have to be readable while Redis is down).
The `error` and `denied` labels exist to be told apart here
(`internal/api/check.go`): `error` means the store failed, `denied` means the
limiter worked.

| rule | result | requests |
|---|---|---|
| `burst-tier-token-bucket` | `error` | 2924 |
| `cached-tier-token-bucket` | `allowed` | 954 |
| `cached-tier-token-bucket` | `denied` | 1934 |

These two scrapes bracket the outage by shell command, not by the marker the
tables above use, so the window can run a few milliseconds wide at each end and
pick up a handful of requests from either side of it. Where the two disagree by
single digits, the timeline is the precise one; this table is here for the
`result` labels, which the timeline cannot see.

And between the start command returning and the end of the run:

| rule | result | requests |
|---|---|---|
| `burst-tier-token-bucket` | `allowed` | 600 |
| `burst-tier-token-bucket` | `error` | 7 |
| `cached-tier-token-bucket` | `allowed` | 589 |
| `cached-tier-token-bucket` | `denied` | 8 |

## Results — counter state across the gap

Authoritative bucket state read from Redis before the stop and after the run.
A `tokens` field that is empty means the key does not exist.

Before the outage:

```
dbsize 2
burst-tier-token-bucket tokens=999 last_refill_ms=1788149792837
cached-tier-token-bucket tokens=999 last_refill_ms=1788149793503
```

After recovery:

```
dbsize 2
burst-tier-token-bucket tokens=999 last_refill_ms=1788149832159
cached-tier-token-bucket tokens=589.4000000000002 last_refill_ms=1788149832153
```

What the restarted Redis read back off disk, sampled the moment it came up:

```
loading:0
async_loading:0
rdb_changes_since_last_save:123
rdb_last_load_keys_expired:0
rdb_last_load_keys_loaded:2
```

This last block is load-bearing, because the bucket state on its own cannot
answer whether the dataset survived: a token bucket whose key was lost is
recreated at capacity, and one whose key survived refills to capacity across any
gap longer than `capacity / rate` = 5.0s. Both roads lead to a full bucket, so for
this algorithm and this outage length the two are indistinguishable from the
outside. `rdb_last_load_keys_loaded` is the direct answer.

**Expect this one figure to differ between runs, and do not treat a change in it
as a regression.** `docker compose stop -t 0` sends SIGTERM and follows it with
SIGKILL immediately, while a stock `redis:7-alpine` has save points configured
and so tries to write its dataset out on SIGTERM. Which one wins is a race, and
this harness has been observed landing on both sides of it: repeated stop/start
cycles on an idle machine lost the dataset every time, while a run under the
full two-generator load reloaded it. The finding is the race itself — an
unpersisted store abruptly stopped guarantees nothing either way — not whichever
side this particular run came down on.

The cached arm admitted **999** requests locally during the outage. Its node
could not flush any of them while the store was gone, and a failed flush leaves
the pending delta intact (`rules.compileBatchingTokenBucket`), so those admits
should all be charged to Redis by the first flush that succeeds after recovery
rather than lost — the cached arm's token count after recovery, against the
uncached arm's as a control, is where to read that off.

## Escalation check

Both arms degraded as designed: the uncached arm failed with the 503 that
`internal/api/check.go` reserves for a store error rather than a rate-limit
denial, the cached arm spent its local baseline within a factor of two of the
expected 10.0s and then denied rather than errored, no cached request produced a
store error, and both arms served 200s again after recovery. This does not
replace human judgement of the tables above.

Raw vegeta output, per-request timelines, and the metric and Redis snapshots are
kept under `bench/results/raw/2026-08-31-111538-failure-mode/` (gitignored — regenerate by
rerunning this script rather than diffing binary blobs).
