#!/usr/bin/env bash
#
# bench/run-outage.sh — Redis outage harness.
#
# Failure-mode behaviour is the one axis of this project the other harnesses
# never touch: what a node actually does while the store it fails closed on
# is gone, and what it does when the store comes back. This script kills
# Redis (SIGKILL — no RDB save on the way out, so whatever was in memory is
# lost unless a snapshot already happened to exist) in the middle of a
# steady, well-under-limit stream of requests, restarts it a fixed number of
# seconds later, and records per second what the API returned throughout.
#
# Two arms, same as bench/run-overshoot.sh, because they are expected to
# degrade differently and the difference is the finding:
#
#   burst-tier-token-bucket  (local_cache: false) — every decision is a Lua
#     round-trip, so the moment Redis is unreachable every request should
#     fail closed: 503, allowed:false, counted as result="error".
#   cached-tier-token-bucket (local_cache: true)  — decides from its node-local
#     view and only talks to Redis on the flush tick, so it should keep
#     admitting from the balance it last heard about, then deny (429, not
#     503 — the limiter itself is working) once that local view runs dry,
#     because the local cache learns about refills only from a successful
#     sync. Recovery then has to reconcile a large pending delta against
#     whatever state Redis restarted with.
#
# The offered rate is deliberately below the bucket's refill rate so that,
# with Redis healthy, every request is admitted: any non-200 in the timeline
# is the outage, not the limit. The outage is long enough, and the rate high
# enough, that the cached arm's local balance (capacity 1000) is exhausted
# before Redis returns — that transition is the interesting part.
#
# Per CONTEXT.md this is a measurement, not a claim: whatever the timeline
# shows is published as-is, surprises included.
#
# Usage: bench/run-outage.sh [--rate 100] [--duration 50] [--kill-at 15] [--restart-at 35] [--scale 1] [--keep-stack]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

RATE="100"
DURATION="50"
KILL_AT="15"
RESTART_AT="35"
SCALE="1"
KEEP_STACK="0"
INVOCATION=("$@")

while [[ $# -gt 0 ]]; do
	case "$1" in
	--rate)
		RATE="$2"
		shift 2
		;;
	--duration)
		DURATION="$2"
		shift 2
		;;
	--kill-at)
		KILL_AT="$2"
		shift 2
		;;
	--restart-at)
		RESTART_AT="$2"
		shift 2
		;;
	--scale)
		SCALE="$2"
		shift 2
		;;
	--keep-stack)
		KEEP_STACK="1"
		shift
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 1
		;;
	esac
done

command -v docker >/dev/null || {
	echo "docker not found on PATH" >&2
	exit 1
}
command -v jq >/dev/null || {
	echo "jq not found on PATH (brew install jq)" >&2
	exit 1
}
command -v curl >/dev/null || {
	echo "curl not found on PATH" >&2
	exit 1
}

if ((KILL_AT <= 0 || RESTART_AT <= KILL_AT || DURATION <= RESTART_AT)); then
	echo "need 0 < --kill-at ($KILL_AT) < --restart-at ($RESTART_AT) < --duration ($DURATION)" >&2
	exit 1
fi

# The controlled A/B pair (config.example.yaml) — hardcoded because the
# reading of the timeline depends on these exact numbers matching that file.
CAPACITY=1000
REFILL_RATE=200
if ((RATE >= REFILL_RATE)); then
	echo "--rate ($RATE) must be below the pair's refill rate ($REFILL_RATE/s) so a healthy Redis admits every request and only the outage shows in the timeline" >&2
	exit 1
fi
OUTAGE_SECONDS=$((RESTART_AT - KILL_AT))
OUTAGE_REQUESTS=$((RATE * OUTAGE_SECONDS))
if ((OUTAGE_REQUESTS <= CAPACITY)); then
	echo "the outage offers only $OUTAGE_REQUESTS requests, not enough to exhaust the cached arm's local balance (capacity $CAPACITY); raise --rate or widen the --kill-at/--restart-at window" >&2
	exit 1
fi

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP-outage"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG-outage.md"

log() { echo "[bench-outage] $*" >&2; }

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# --- helpers -----------------------------------------------------------
# compute_cpusets and bring_up are the same as bench/run-overshoot.sh's; see
# the notes there. A fresh stack per arm means a fresh Redis AND a fresh
# Prometheus, so the metric totals read back at the end of an arm describe
# that arm alone.

compute_cpusets() {
	local total_cpus
	total_cpus="$(docker info --format '{{.NCPU}}')"
	if ((total_cpus < 2)); then
		echo "need at least 2 CPUs available to Docker to pin the generator away from limigo; have $total_cpus" >&2
		exit 1
	fi
	local half=$((total_cpus / 2))
	LIMIGO_CPUSET="0-$((half - 1))"
	GEN_CPUSET="$half-$((total_cpus - 1))"
	TOTAL_CPUS="$total_cpus"
}

bring_up() {
	local scale="$1"
	log "bringing stack down for a clean start"
	docker compose down --remove-orphans >/dev/null
	log "starting stack: docker compose up -d --build --wait --scale limigo=$scale"
	docker compose up -d --build --wait --scale "limigo=$scale" >/dev/null

	PROJECT="$(docker compose config --format json | jq -r '.name')"
	NETWORK="${PROJECT}_default"

	local cid
	for cid in $(docker compose ps -q limigo); do
		docker update --cpuset-cpus="$LIMIGO_CPUSET" "$cid" >/dev/null
	done
	log "pinned $(docker compose ps -q limigo | wc -l | tr -d ' ') limigo replica(s) to CPUs $LIMIGO_CPUSET; generator uses $GEN_CPUSET"

	log "settling: waiting for Traefik's routing table to catch up to the new replica set"
	sleep 5
}

compute_cpusets

PROXY_URL="http://traefik/v1/check"

# now_epoch — whole seconds. macOS date has no %N and per-second resolution
# is all the timeline below needs. The container clock (Docker Desktop VM)
# tracks the host clock, which is what lets host-side kill/restart stamps be
# placed on the generator's timeline.
now_epoch() { date +%s; }

# prom_totals <rule> — reads limigo_requests_total for one rule back from
# Prometheus, summed across replicas, one "result=count" per line. This is
# the series the fail-closed dashboard panel is built on, so the 503 count
# vegeta saw and the result="error" count Prometheus saw are reported side
# by side: an outage that showed up as resets rather than 503s would make
# the two disagree, which is exactly what #29's deadline exists to prevent.
prom_totals() {
	local rule="$1"
	curl -s --get "http://localhost:9090/api/v1/query" \
		--data-urlencode "query=sum by (result) (limigo_requests_total{rule=\"$rule\"})" |
		jq -r '.data.result[] | "\(.metric.result)=\(.value[1] | tonumber | floor)"' | sort
}

# run_outage_arm <plan> <rule> <name> — one full timeline for one arm:
# start the generator detached, kill Redis at KILL_AT, restart it at
# RESTART_AT, wait for the generator, then decode its results into a
# per-second table. Emits nothing on stdout; fills the ARM_* variables.
run_outage_arm() {
	local plan="$1" rule="$2" name="$3"
	local targets="$RAW_DIR/$name.jsonl"
	bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "$plan" 1 "$targets"

	log "=== arm $name (X-Plan: $plan) ==="
	bring_up "$SCALE"

	log "starting generator: ${RATE}/1s for ${DURATION}s, single key"
	local container
	container="$(docker run -d \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$targets")" -format=json \
		-rate="${RATE}/1s" -duration="${DURATION}s" \
		-output="/raw/$name.bin")"
	local t0
	t0="$(now_epoch)"

	sleep "$KILL_AT"
	log "t+${KILL_AT}s: docker compose kill -s SIGKILL redis"
	docker compose kill -s SIGKILL redis >/dev/null 2>&1
	local killed_at
	killed_at="$(now_epoch)"

	sleep "$OUTAGE_SECONDS"
	log "t+${RESTART_AT}s: docker compose start redis"
	docker compose start redis >/dev/null 2>&1
	local restarted_at
	restarted_at="$(now_epoch)"

	log "waiting for the generator to finish"
	local exit_code
	exit_code="$(docker wait "$container")"
	docker rm "$container" >/dev/null
	if [[ "$exit_code" != "0" ]]; then
		echo "FATAL: generator exited $exit_code for $name — check $RAW_DIR/$name.bin" >&2
		exit 1
	fi

	# Prometheus scrapes every 5s (prometheus/prometheus.yml); give the last
	# scrape time to land before reading totals back.
	sleep 10
	ARM_PROM="$(prom_totals "$rule")"

	# Decode to JSON lines, then bucket by whole second since the first
	# request. vegeta stamps RFC3339Nano in UTC; jq's fromdateiso8601 wants
	# whole seconds and a Z suffix, hence the sub.
	docker run --rm -v "$RAW_DIR:/raw" limigo-bench-vegeta encode -to=json "/raw/$name.bin" >"$RAW_DIR/$name.jsonl.out"
	local first_epoch
	first_epoch="$(jq -r '.timestamp | sub("\\.[0-9]+"; "") | fromdateiso8601' "$RAW_DIR/$name.jsonl.out" | sort -n | head -1)"

	# Per-second rows: second|200|429|503|other. "other" is anything else,
	# including code 0 (vegeta's marker for a request that got no HTTP
	# response at all — a reset or a timeout), which is the shape the
	# fail-closed path must never produce.
	jq -r --argjson t0 "$first_epoch" '
		((.timestamp | sub("\\.[0-9]+"; "") | fromdateiso8601) - $t0) as $s
		| "\($s) \(.code)"' "$RAW_DIR/$name.jsonl.out" |
		awk -v duration="$DURATION" '
			{ n[$1]++; if ($2 == 200) a[$1]++; else if ($2 == 429) d[$1]++; else if ($2 == 503) e[$1]++; else o[$1]++ }
			END { for (s = 0; s < duration; s++) printf "%d|%d|%d|%d|%d\n", s, a[s] + 0, d[s] + 0, e[s] + 0, o[s] + 0 }' \
			>"$RAW_DIR/$name.seconds"

	ARM_NAME="$name"
	ARM_KILL_OFFSET=$((killed_at - first_epoch))
	ARM_RESTART_OFFSET=$((restarted_at - first_epoch))
	ARM_T0_HOST_OFFSET=$((t0 - first_epoch))
	ARM_TOTALS="$(awk -F'|' '{ a += $2; d += $3; e += $4; o += $5 } END { printf "%d|%d|%d|%d", a, d, e, o }' "$RAW_DIR/$name.seconds")"

	# Flip: first second at or after the kill with any non-200. Recovery:
	# first second at or after the restart from which every remaining second
	# is all-200 (an all-idle second — no requests at all — counts as
	# recovered only if a later second with traffic is all-200 too).
	ARM_FLIP="$(awk -F'|' -v k="$ARM_KILL_OFFSET" '$1 >= k && ($3 + $4 + $5) > 0 { print $1; exit }' "$RAW_DIR/$name.seconds")"
	ARM_RECOVER="$(awk -F'|' -v r="$ARM_RESTART_OFFSET" '
		{ sec[NR] = $1; bad[NR] = $3 + $4 + $5; n = NR }
		END {
			last_bad = -1
			for (i = 1; i <= n; i++) if (sec[i] >= r && bad[i] > 0) last_bad = sec[i]
			if (last_bad < 0) { print r; exit }
			for (i = 1; i <= n; i++) if (sec[i] > last_bad) { print sec[i]; exit }
			print ""
		}' "$RAW_DIR/$name.seconds")"

	# Collapse the per-second table into runs of identical shape (which of
	# the four columns are non-zero), so a 50-second timeline reads as a
	# handful of phases rather than fifty rows. Totals per run.
	ARM_RUNS="$(awk -F'|' '
		function shape(a, d, e, o) { return (a > 0) "" (d > 0) "" (e > 0) "" (o > 0) }
		function emit() { if (start >= 0) printf "%d–%d|%d|%d|%d|%d\n", start, prev, ra, rd, re, ro }
		BEGIN { start = -1 }
		{
			sh = shape($2, $3, $4, $5)
			if (sh != cur) { emit(); cur = sh; start = $1; ra = rd = re = ro = 0 }
			prev = $1; ra += $2; rd += $3; re += $4; ro += $5
		}
		END { emit() }' "$RAW_DIR/$name.seconds")"
}

# --- run both arms -----------------------------------------------------------
run_outage_arm "burst" "burst-tier-token-bucket" "uncached"
UNCACHED_RUNS="$ARM_RUNS" UNCACHED_TOTALS="$ARM_TOTALS" UNCACHED_PROM="$ARM_PROM"
UNCACHED_KILL="$ARM_KILL_OFFSET" UNCACHED_RESTART="$ARM_RESTART_OFFSET"
UNCACHED_FLIP="$ARM_FLIP" UNCACHED_RECOVER="$ARM_RECOVER" UNCACHED_SKEW="$ARM_T0_HOST_OFFSET"

run_outage_arm "cached" "cached-tier-token-bucket" "cached"
CACHED_RUNS="$ARM_RUNS" CACHED_TOTALS="$ARM_TOTALS" CACHED_PROM="$ARM_PROM"
CACHED_KILL="$ARM_KILL_OFFSET" CACHED_RESTART="$ARM_RESTART_OFFSET"
CACHED_FLIP="$ARM_FLIP" CACHED_RECOVER="$ARM_RECOVER" CACHED_SKEW="$ARM_T0_HOST_OFFSET"

# latency_or_never <event-second> <reference-second> — "Ns after" or "never".
latency_or_never() {
	if [[ -z "$1" ]]; then
		echo "never"
	else
		echo "$(($1 - $2))s after"
	fi
}

# --- write results file ------------------------------------------------------
write_arm() {
	local label="$1" runs="$2" totals="$3" prom="$4" kill="$5" restart="$6" flip="$7" recover="$8" skew="$9"
	local a d e o
	IFS='|' read -r a d e o <<<"$totals"
	echo "### $label"
	echo
	echo "Redis killed at **t+${kill}s**, restarted at **t+${restart}s** on the generator's"
	echo "timeline (t+0 is its first request; the host stamped the generator's launch at"
	echo "$(printf 't%+ds' "$skew"), so host and container clocks agree to within a second)."
	echo
	echo "| seconds | 200 allowed | 429 denied | 503 error | other (no HTTP response) |"
	echo "|---|---|---|---|---|"
	while IFS='|' read -r range ra rd re ro; do
		echo "| $range | $ra | $rd | $re | $ro |"
	done <<<"$runs"
	echo "| **total** | **$a** | **$d** | **$e** | **$o** |"
	echo
	echo "- First non-200 after the kill: **$(latency_or_never "$flip" "$kill")** (t+${flip:-—}s)"
	echo "- All-200 again after the restart: **$(latency_or_never "$recover" "$restart")** (t+${recover:-—}s)"
	echo "- \`limigo_requests_total\` by result, read back from Prometheus after the run:"
	while IFS= read -r line; do
		[[ -n "$line" ]] && echo "  - \`$line\`"
	done <<<"$prom"
	echo
}

{
	echo "# Redis outage harness — $STAMP"
	echo
	echo "## Hardware"
	echo
	echo "- Host: \`$(uname -a)\`"
	if command -v sysctl >/dev/null && sysctl -n machdep.cpu.brand_string >/dev/null 2>&1; then
		echo "- CPU: \`$(sysctl -n machdep.cpu.brand_string)\` ($TOTAL_CPUS logical cores)"
	else
		echo "- CPU: $TOTAL_CPUS logical cores"
	fi
	echo "- Docker: \`$(docker version --format '{{.Server.Version}}')\`"
	echo
	echo "## Command"
	echo
	echo '```'
	if ((${#INVOCATION[@]} > 0)); then
		echo "bench/run-outage.sh ${INVOCATION[*]}"
	else
		echo "bench/run-outage.sh"
	fi
	echo '```'
	echo
	echo "## Method"
	echo
	echo "One key, **${RATE} req/s for ${DURATION}s** against each arm of \`config.example.yaml\`'s"
	echo "\`burst-tier-token-bucket\` / \`cached-tier-token-bucket\` pair (capacity $CAPACITY,"
	echo "refill ${REFILL_RATE}/s, differing only in \`local_cache\`), $SCALE Limigo replica(s) behind"
	echo "Traefik, fresh stack per arm. The offered rate is below the refill rate, so with"
	echo "Redis healthy every request is admitted and any non-200 is the outage."
	echo
	echo "At **t+${KILL_AT}s** Redis is killed with SIGKILL (\`docker compose kill -s SIGKILL redis\`:"
	echo "no snapshot on the way out — in-memory state is lost unless an RDB save had"
	echo "already happened). At **t+${RESTART_AT}s** it is started again"
	echo "(\`docker compose start redis\`), a **${OUTAGE_SECONDS}s outage** during which"
	echo "**$OUTAGE_REQUESTS requests** are offered — more than the cached arm's local balance"
	echo "(capacity $CAPACITY) can cover, so its transition from local admits to local"
	echo "denials falls inside the window. Responses are bucketed per second from"
	echo "vegeta's own timestamps; the kill and restart are stamped on the host and"
	echo "placed on that timeline."
	echo
	echo "Reading the columns: **503** is the fail-closed path (store unreachable,"
	echo "\`result=\"error\"\`); **429** is a denial by a limiter that is working"
	echo "(\`result=\"denied\"\`); **other** is a request that got no HTTP response, which"
	echo "the fail-closed path must never produce — the per-request deadline on"
	echo "\`/v1/check\` exists to keep this column at zero."
	echo
	echo "## Results"
	echo
	write_arm "local_cache: false (burst-tier-token-bucket)" "$UNCACHED_RUNS" "$UNCACHED_TOTALS" "$UNCACHED_PROM" "$UNCACHED_KILL" "$UNCACHED_RESTART" "$UNCACHED_FLIP" "$UNCACHED_RECOVER" "$UNCACHED_SKEW"
	write_arm "local_cache: true (cached-tier-token-bucket)" "$CACHED_RUNS" "$CACHED_TOTALS" "$CACHED_PROM" "$CACHED_KILL" "$CACHED_RESTART" "$CACHED_FLIP" "$CACHED_RECOVER" "$CACHED_SKEW"
	echo "## What to check before publishing"
	echo
	echo "- The **other** column is zero in both arms. A non-zero value means some"
	echo "  requests were reset or timed out instead of receiving the fail-closed 503;"
	echo "  that is a defect in the deadline path, not a property of the outage."
	echo "- The uncached arm's **503** total matches its Prometheus \`error\` count, and"
	echo "  the cached arm's **429** total matches its \`denied\` count. The API and the"
	echo "  dashboard must tell the same story."
	echo "- Whether the cached arm's post-restart phase shows a stretch of 429s: that is"
	echo "  the pending local delta being reconciled against a Redis that came back"
	echo "  empty (state lost) or full (state kept), and which one happened is a"
	echo "  finding to write down, not to smooth over."
	echo
	echo "Raw vegeta output, decoded JSON lines, and per-second buckets are kept under"
	echo "\`bench/results/raw/$STAMP-outage/\` (gitignored — regenerate by rerunning this"
	echo "script rather than diffing binary blobs)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"

if [[ "$KEEP_STACK" != "1" ]]; then
	log "bringing stack down"
	docker compose down --remove-orphans >/dev/null
fi
