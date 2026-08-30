#!/usr/bin/env bash
#
# bench/run-flush-sweep.sh — flush-interval accuracy/latency sweep (ADR-0005).
#
# bench/run-overshoot.sh established *that* local caching overshoots the
# configured limit. This turns the flush interval — the
# tunable knob on that trade-off (CONTEXT.md: "flush interval") — into a
# curve: at each interval, how much accuracy do you buy, and what does it
# cost in latency.
#
# Uses the same controlled config.example.yaml pair as the overshoot harness
# (cached-tier-token-bucket, capacity 1000, rate 200/s, local_cache: true).
# Only the cached arm is swept: the uncached arm never reads the flush
# interval at all (it makes no local admission decisions to reconcile), so
# sweeping it would just repeat the same number four times.
#
# The flush interval is set per run via LIMIGO_CACHE_FLUSH_INTERVAL, which
# docker-compose.yml now passes through from this script's shell environment
# (default: unset -> blank -> main.go's own 10ms default).
#
# Each (node count, interval) point fires the same fixed, over-driven request
# count the overshoot harness uses, at a single key, so the resulting overshoot is
# comparable across the whole sweep and computed against the same
# hardware-independent expected ceiling. That offered load denies most
# requests once the bucket is exhausted (CONTEXT.md: denial path is cheap,
# often no Redis round-trip). Blending admitted and denied latencies into one
# percentile would measure the cheap path and mislabel it as the cost of
# flushing (CONTEXT.md: "a benchmark that denies most requests is measuring
# the cheap path and must be labelled as such") — so latency here is
# computed from the admitted (allow-path) responses only, decoded from the
# raw vegeta results rather than read off vegeta's own aggregate percentiles.
#
# Usage: bench/run-flush-sweep.sh [--nodes "3 1"] [--intervals "1ms 10ms 100ms 1000ms"]
#                                  [--requests 6000] [--seconds 3] [--max-workers 500] [--keep-stack]
#
# The first entry in --nodes is the primary, fixed-node-count sweep. Any
# further entries are additional fixed-node-count sweeps run the same way,
# reusing the same harness, to show how the curve shifts with node count.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

NODES_ARG="3 1"
INTERVALS_ARG="1ms 10ms 100ms 1000ms"
REQUESTS="6000"
SECONDS_WINDOW="3"
MAX_WORKERS="500"
KEEP_STACK="0"
INVOCATION=("$@")

while [[ $# -gt 0 ]]; do
	case "$1" in
	--nodes)
		NODES_ARG="$2"
		shift 2
		;;
	--intervals)
		INTERVALS_ARG="$2"
		shift 2
		;;
	--requests)
		REQUESTS="$2"
		shift 2
		;;
	--seconds)
		SECONDS_WINDOW="$2"
		shift 2
		;;
	--max-workers)
		MAX_WORKERS="$2"
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

if ((REQUESTS % SECONDS_WINDOW != 0)); then
	echo "--requests ($REQUESTS) must be evenly divisible by --seconds ($SECONDS_WINDOW), so the rate passed to vegeta is a whole number" >&2
	exit 1
fi
RATE=$((REQUESTS / SECONDS_WINDOW))

read -r -a NODE_COUNTS <<<"$NODES_ARG"
read -r -a INTERVALS <<<"$INTERVALS_ARG"
if ((${#INTERVALS[@]} < 4)); then
	echo "need at least 4 flush intervals to sweep (got ${#INTERVALS[@]}: $INTERVALS_ARG)" >&2
	exit 1
fi

# The controlled A/B pair (config.example.yaml), same as the overshoot
# harness — hardcoded because the expected-ceiling comparison depends on
# these exact numbers matching that file.
CAPACITY=1000
REFILL_RATE=200
EXPECTED_CEILING=$((CAPACITY + REFILL_RATE * SECONDS_WINDOW))

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP-flush-sweep"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG-flush-sweep.md"

log() { echo "[bench-flush-sweep] $*" >&2; }

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# compute_cpusets — see bench/run-overshoot.sh for the identical helper.
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

# bring_up <scale-n> <flush-interval> — clean-starts the default stack
# (config.example.yaml, baked into the image — no compose override, same as
# the overshoot harness) scaled to N limigo replicas, with LIMIGO_CACHE_FLUSH_INTERVAL
# exported into this shell so docker-compose.yml's variable substitution
# picks it up. A fresh stack means a fresh, empty Redis every time: state
# from a previous interval, node count, never leaks into the next point.
bring_up() {
	local scale="$1" interval="$2"
	export LIMIGO_CACHE_FLUSH_INTERVAL="$interval"

	log "bringing stack down for a clean start"
	docker compose down --remove-orphans >/dev/null
	log "starting stack: LIMIGO_CACHE_FLUSH_INTERVAL=$interval docker compose up -d --build --wait --scale limigo=$scale"
	docker compose up -d --build --wait --scale "limigo=$scale" >/dev/null

	PROJECT="$(docker compose config --format json | jq -r '.name')"
	NETWORK="${PROJECT}_default"

	local cid
	for cid in $(docker compose ps -q limigo); do
		docker update --cpuset-cpus="$LIMIGO_CPUSET" "$cid" >/dev/null
	done
	log "pinned $(docker compose ps -q limigo | wc -l | tr -d ' ') limigo replica(s) to CPUs $LIMIGO_CPUSET; generator uses $GEN_CPUSET; flush interval $interval"

	# See bench/run-throughput.sh bring_up for why this settle wait exists:
	# Traefik's routing table lags the replica set by a few seconds.
	log "settling: waiting for Traefik's routing table to catch up to the new replica set"
	sleep 5
}

compute_cpusets

PROXY_URL="http://traefik/v1/check"

# run_attack <name> — fires exactly REQUESTS requests at a single key
# (cardinality 1: single-bucket contention, same as the overshoot harness) against the
# cached-tier-token-bucket rule, over SECONDS_WINDOW seconds.
run_attack() {
	local name="$1"
	local targets="$RAW_DIR/$name.jsonl"
	bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "cached" 1 "$targets"
	log "running $name: $REQUESTS requests over ${SECONDS_WINDOW}s (rate ${RATE}/1s), single key"
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$targets")" -format=json \
		-rate="${RATE}/1s" -duration="${SECONDS_WINDOW}s" -max-workers="$MAX_WORKERS" \
		-output="/raw/$name.bin"
}

# report_json <name> — vegeta's own aggregate report, used only for the
# request/status-code counts (overshoot side of the measurement). Latency
# comes from decode_admitted_latencies below instead — see file header.
report_json() {
	local name="$1"
	docker run --rm -v "$RAW_DIR:/raw" limigo-bench-vegeta report -type=json "/raw/$name.bin"
}

# decode_admitted_latencies <name> — decodes the raw vegeta results and
# writes one admitted (code=200) latency per line, in milliseconds, sorted
# ascending, to $RAW_DIR/<name>.admitted-ms. Used to compute allow-path-only
# percentiles (file header: denial-path latency would misrepresent the flush
# interval's cost).
decode_admitted_latencies() {
	local name="$1"
	docker run --rm -v "$RAW_DIR:/raw" limigo-bench-vegeta encode -to json "/raw/$name.bin" |
		jq -r 'select(.code == 200) | .latency / 1e6' |
		sort -n >"$RAW_DIR/$name.admitted-ms"
}

# percentile <sorted-ms-file> <p> — nearest-rank percentile (p in 0-100) off
# a file of one ascending numeric value per line. Prints "n/a" on an empty
# file rather than dividing by zero.
percentile() {
	local file="$1" p="$2" n
	n="$(wc -l <"$file" | tr -d ' ')"
	if ((n == 0)); then
		echo "n/a"
		return
	fi
	awk -v p="$p" -v n="$n" 'BEGIN {
		idx = int((p / 100) * n)
		if (idx < 1) idx = 1
		if (idx > n) idx = n
	}
	NR == idx { print; exit }' "$file"
}

# measure <node> <interval> <name> — brings up the stack at the given node
# count and flush interval, runs the attack, and emits one pipe-separated
# result line on stdout:
#   node|interval|sent|admitted|denied|other|ceiling|overshoot_pct|admitted_samples|p50|p95|p99|max
measure() {
	local node="$1" interval="$2" name="$3"
	run_attack "$name"

	local json sent admitted denied other overshoot_pct min_acceptable
	json="$(report_json "$name")"
	sent="$(jq -r '.requests' <<<"$json")"

	min_acceptable=$((REQUESTS * 90 / 100))
	if ((sent < min_acceptable)); then
		echo "FATAL: $name only sent $sent of the targeted $REQUESTS requests (<90%) — offered load no longer reliably swamps the expected ceiling of $EXPECTED_CEILING, refusing to publish this run. Try a lower --rate or --max-workers." >&2
		exit 1
	fi
	if ((sent != REQUESTS)); then
		log "note: $name sent $sent of the targeted $REQUESTS requests (vegeta pacer scheduling slop under load) — using the actual count"
	fi

	admitted="$(jq -r '.status_codes["200"] // 0' <<<"$json")"
	denied="$(jq -r '.status_codes["429"] // 0' <<<"$json")"
	other=$((sent - admitted - denied))
	if ((other != 0)); then
		log "WARNING: $name saw $other responses that were neither 200 nor 429 — check $RAW_DIR/$name.bin"
	fi

	overshoot_pct="$(awk -v admitted="$admitted" -v ceiling="$EXPECTED_CEILING" 'BEGIN { printf "%.2f", ((admitted - ceiling) / ceiling) * 100 }')"

	decode_admitted_latencies "$name"
	local admitted_samples p50 p95 p99 max
	admitted_samples="$(wc -l <"$RAW_DIR/$name.admitted-ms" | tr -d ' ')"
	p50="$(percentile "$RAW_DIR/$name.admitted-ms" 50)"
	p95="$(percentile "$RAW_DIR/$name.admitted-ms" 95)"
	p99="$(percentile "$RAW_DIR/$name.admitted-ms" 99)"
	max="$(percentile "$RAW_DIR/$name.admitted-ms" 100)"

	echo "$node|$interval|$sent|$admitted|$denied|$other|$EXPECTED_CEILING|$overshoot_pct|$admitted_samples|$p50|$p95|$p99|$max"
}

# row_field <row> <field-number> — field numbering matches the measure()
# echo above.
row_field() { cut -d'|' -f"$2" <<<"$1"; }

# summarize_sweep <node> <rows...> — states the actual observed direction
# and rough slope in prose, rather than leaving it for the reader to infer,
# comparing the shortest and longest flush interval in the sweep and noting
# the biggest single step, rather than just the two endpoints.
summarize_sweep() {
	local node="$1"
	shift
	local rows=("$@")
	local first="${rows[0]}" last="${rows[$((${#rows[@]} - 1))]}"
	local first_iv first_pct first_p99 last_iv last_pct last_p99
	first_iv="$(row_field "$first" 2)"
	first_pct="$(row_field "$first" 8)"
	first_p99="$(row_field "$first" 12)"
	last_iv="$(row_field "$last" 2)"
	last_pct="$(row_field "$last" 8)"
	last_p99="$(row_field "$last" 12)"

	awk -v node="$node" -v first_iv="$first_iv" -v first_pct="$first_pct" -v first_p99="$first_p99" \
		-v last_iv="$last_iv" -v last_pct="$last_pct" -v last_p99="$last_p99" 'BEGIN {
		a_first = (first_pct < 0 ? -first_pct : first_pct)
		a_last = (last_pct < 0 ? -last_pct : last_pct)
		dir = (a_last > a_first) ? "widens" : "tightens"
		printf "node=%s: |overshoot| %s from %.2f%% at %s to %.2f%% at %s (%.1fx). ", \
			node, dir, a_first, first_iv, a_last, last_iv, (a_first > 0 ? a_last / a_first : a_last / 0.01)
		if (last_p99 == "n/a" || first_p99 == "n/a") {
			printf "admitted-path p99 latency not comparable — too few admitted samples at one end.\n"
		} else {
			lat_dir = (last_p99 + 0 > first_p99 + 0) ? "rises" : "falls"
			printf "admitted-path p99 latency %s from %.3fms at %s to %.3fms at %s.\n", \
				lat_dir, first_p99, first_iv, last_p99, last_iv
		}
	}'
}

# check_curve <label> <rows...> — flat-or-non-monotonic investigation
# reported rather than smoothed over. "Flat" = the |overshoot| range
# across the whole sweep is small relative to the configured limit. Sweeps
# only ever have as many points as --intervals, so this walks the row array
# rather than assuming exactly 3 or 4 rows.
check_curve() {
	local label="$1"
	shift
	local rows=("$@")
	local -a vals=()
	local r
	for r in "${rows[@]}"; do
		vals+=("$(row_field "$r" 8)")
	done

	awk -v label="$label" 'BEGIN {
		n = split(ARGV[1], vals, ",")
		min = 1e18; max = -1e18
		monotonic = 1
		prev = ""
		for (i = 1; i <= n; i++) {
			v = vals[i] + 0
			a = (v < 0 ? -v : v)
			if (a < min) min = a
			if (a > max) max = a
			if (prev != "" && a + 0.01 < prev) monotonic = 0
			prev = a
		}
		range = max - min
		if (range < 2.0) {
			printf "%s: flat — |overshoot| ranges only %.2f to %.2f%% of the configured limit across the whole interval sweep, not tightening as the interval shortens as expected\n", label, min, max
		} else if (!monotonic) {
			printf "%s: non-monotonic — |overshoot| does not consistently grow as the flush interval lengthens (see table)\n", label
		}
	}' "$(
		IFS=,
		echo "${vals[*]}"
	)"
}

# --- run the sweep(s) --------------------------------------------------
# ROWS_BY_NODE[i] holds the pipe-joined rows (newline-separated) for
# NODE_COUNTS[i]. Plain indexed array, not associative — see
# bench/run-throughput.sh for why (bash 3.2 on macOS).
ROWS_BY_NODE=()
CURVE_NOTES=()
SLOPE_SUMMARIES=()

for node in "${NODE_COUNTS[@]}"; do
	log "=== node count $node ==="
	rows=()
	for interval in "${INTERVALS[@]}"; do
		bring_up "$node" "$interval"
		name="n${node}-${interval}"
		rows+=("$(measure "$node" "$interval" "$name")")
	done
	note="$(check_curve "node=$node" "${rows[@]}")"
	if [[ -n "$note" ]]; then
		CURVE_NOTES+=("$note")
	fi
	SLOPE_SUMMARIES+=("$(summarize_sweep "$node" "${rows[@]}")")
	ROWS_BY_NODE+=("$(printf '%s\n' "${rows[@]}")")
done

PRIMARY_NODE="${NODE_COUNTS[0]}"

# --- write results file ------------------------------------------------------
{
	echo "# Flush-interval accuracy/latency sweep — $STAMP"
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
		echo "bench/run-flush-sweep.sh ${INVOCATION[*]}"
	else
		echo "bench/run-flush-sweep.sh"
	fi
	echo '```'
	echo
	echo "## Method"
	echo
	echo "\`config.example.yaml\`'s \`cached-tier-token-bucket\` (capacity $CAPACITY, rate"
	echo "${REFILL_RATE}/s, \`local_cache: true\`) — the same rule the overshoot harness uses. Only the"
	echo "cached arm is swept; the uncached arm doesn't read the flush interval at all."
	echo
	echo "At every point, exactly **$REQUESTS requests** are fired at a single key over"
	echo "**${SECONDS_WINDOW}s** (rate ${RATE}/1s), against a fresh stack (empty Redis,"
	echo "no state carried over from the previous point). Expected ceiling = capacity +"
	echo "rate * seconds = $CAPACITY + $REFILL_RATE * $SECONDS_WINDOW = **$EXPECTED_CEILING**"
	echo "— the most a correct, atomic token bucket can admit over the window regardless"
	echo "of node count or flush interval. Overshoot = (admitted - $EXPECTED_CEILING) /"
	echo "$EXPECTED_CEILING, as a percentage."
	echo
	echo "This offered load exceeds the ceiling on purpose (as in the overshoot harness), so most"
	echo "requests are denied once the bucket empties. Denial-path latency is cheap and"
	echo "unrelated to the flush interval (CONTEXT.md), so latency percentiles below are"
	echo "computed from admitted (allow-path) responses only, decoded from the raw vegeta"
	echo "results rather than read off vegeta's own blended aggregate — the admitted-sample"
	echo "count is reported alongside so a thin sample is visible, not hidden."
	echo
	echo "Node count is held fixed within each sweep below; the flush interval is the only"
	echo "variable. The first sweep (node=$PRIMARY_NODE) is the primary result; further"
	echo "sweeps at other node counts show how the curve shifts with node count."
	echo

	for i in "${!NODE_COUNTS[@]}"; do
		node="${NODE_COUNTS[$i]}"
		echo "## Sweep — node=$node"
		echo
		echo "| flush interval | sent | admitted | denied | other | ceiling | overshoot | admitted samples | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |"
		echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
		while IFS= read -r row; do
			[[ -z "$row" ]] && continue
			IFS='|' read -r rnode interval sent admitted denied other ceiling pct samples p50 p95 p99 max <<<"$row"
			echo "| $interval | $sent | $admitted | $denied | $other | $ceiling | ${pct}% | $samples | $p50 | $p95 | $p99 | $max |"
		done <<<"${ROWS_BY_NODE[$i]}"
		echo
	done

	echo "## Direction and slope"
	echo
	echo "Expectation (ADR-0005, CONTEXT.md): a shorter flush interval reconciles the"
	echo "local cache with Redis more often, so it should mean tighter accuracy (smaller"
	echo "|overshoot|) at the cost of more frequent Redis round-trips — which should show"
	echo "up as *lower* admitted-path latency being harder to sustain, or as latency rising"
	echo "at the short end of the sweep if the flush goroutine starts contending with the"
	echo "request path. Observed, comparing the shortest and longest interval in each sweep:"
	echo
	for summary in "${SLOPE_SUMMARIES[@]}"; do
		echo "- $summary"
	done
	echo
	if ((${#CURVE_NOTES[@]} > 0)); then
		echo "## Investigation"
		echo
		printf '%s\n\n' "${CURVE_NOTES[@]}"
		echo "A flat or non-monotonic curve is a finding — it would suggest"
		echo "the overshoot has a floor set by something other than flush timing (e.g. the"
		echo "burst-then-debt shared-counter behaviour bench/run-overshoot.sh's escalation"
		echo "check already watches for) — and is reported here rather than smoothed over."
	else
		echo "No flat or non-monotonic curve detected in any sweep above (heuristic: total"
		echo "|overshoot| range across the sweep, and consecutive-point ordering — see"
		echo "script). Read the tables directly for the actual slope; this check only flags"
		echo "sweeps worth a closer look, it does not replace reading them."
	fi
	echo
	echo "Raw vegeta output, generated target files, and the decoded admitted-latency"
	echo "samples are kept under \`bench/results/raw/$STAMP-flush-sweep/\` (gitignored —"
	echo "regenerate by rerunning this script rather than diffing binary blobs)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"
if ((${#CURVE_NOTES[@]} > 0)); then
	for note in "${CURVE_NOTES[@]}"; do
		log "FINDING: $note"
	done
fi

if [[ "$KEEP_STACK" != "1" ]]; then
	log "bringing stack down"
	docker compose down --remove-orphans >/dev/null
fi
