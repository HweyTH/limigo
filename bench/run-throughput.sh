#!/usr/bin/env bash
#
# bench/run-throughput.sh — throughput/latency measurement suite.
#
# Produces the algorithm comparison table and the scaling curve, both opening
# with a fresh set of the three control rows so every
# limiter number in this file can be read as a cost relative to the ceiling,
# not a bare absolute.
#
# Five run groups, each against POST /v1/check through Traefik (in-network —
# the primary number per CONTEXT.md), except group 1 which hits /healthz:
#
#   1. Control rows       — GET /healthz, 1 replica, no rule logic.
#   2. Algorithm axis      — all four algorithms, allow path, 1 replica, at
#                            both 1 key and 10k keys (CONTEXT.md, cardinality).
#   3. Denial axis         — the load-test-deny rule, 1 replica, labelled
#                            separately from the allow path (CONTEXT.md).
#   4. Node axis           — token_bucket, 10k keys, allow path, at 1/2/3
#                            replicas (scaling curve).
#   5. Cached vs uncached  — config.example.yaml's burst-tier/cached-tier
#                            token-bucket pair, held to a fixed rate under
#                            their shared 200/s refill so the allow path stays
#                            hot for both arms; the comparison is latency
#                            (fewer Redis round-trips), not max throughput.
#
# Throughput and latency are never read off the same run — see THROUGHPUT_FLAGS
# and latency_flags below, and the "Measurement model" section this writes into
# the results file. Group 1 additionally establishes a Redis-side bound with
# redis-benchmark against the embedded token_bucket.lua, so an uncached row's
# cost can be attributed to Limigo or to Redis rather than left unexplained.
#
# Usage: bench/run-throughput.sh [--duration 30s] [--max-workers 200] [--keep-stack]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

DURATION="30s"
MAX_WORKERS="200"
KEEP_STACK="0"
INVOCATION=("$@")

while [[ $# -gt 0 ]]; do
	case "$1" in
	--duration)
		DURATION="$2"
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
command -v vegeta >/dev/null || {
	echo "vegeta not found on PATH — needed for the control rows' host-origin leg (brew install vegeta)" >&2
	exit 1
}

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP-throughput"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG-throughput.md"

log() { echo "[bench-throughput] $*" >&2; }

# Two measurement models, deliberately separated (see the "Measurement model"
# section written into the results file):
#
# THROUGHPUT_FLAGS is the closed-loop, max-throughput model: -rate=0 means
# "send as fast as replies come back", and -max-workers caps the pool at a
# fixed concurrency. That is the right tool for "how many req/s can this
# sustain", and the wrong tool for latency. With a capped pool, every worker
# blocked on a slow response is a worker not sending the requests it was due to
# send, so the requests that would have been slowest are never issued and never
# timed — the stall erases its own evidence. Gil Tene named this coordinated
# omission. vegeta does timestamp at actual send time and does grow its worker
# pool to catch up when it falls behind, which is what normally saves it here;
# capping -max-workers removes exactly that escape hatch.
#
# So latency percentiles are never read off a THROUGHPUT_FLAGS run. They come
# from latency_flags: an open-model run pinned to a fixed arrival rate below
# the measured ceiling, with no -max-workers cap, so vegeta is free to spawn
# workers and keep the schedule through a stall instead of coordinating with it.
THROUGHPUT_FLAGS=(-duration="$DURATION" -rate=0 -max-workers="$MAX_WORKERS")

# latency_flags <rate> — open-model attack flags at a fixed arrival rate.
# Deliberately omits -max-workers so the pool is vegeta's unbounded default.
latency_flags() {
	echo -duration="$DURATION" -rate="$1"
}

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# --- helpers shared across every run group --------------------------------

# compute_cpusets — splits the CPUs Docker itself reports in half: one half
# for the limigo replica(s), the other for the generator, so they are never
# competing for the same cores. Sets LIMIGO_CPUSET and GEN_CPUSET.
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

# bring_up <scale-n> [override-compose-file] — clean-starts the stack (base
# docker-compose.yml, plus the override file when given), scaled to N limigo
# replicas, pins every limigo replica to LIMIGO_CPUSET, and sets NETWORK to
# the compose network name. -f must precede the "up" subcommand for docker
# compose, so the file list is built once here rather than left to callers.
bring_up() {
	local scale="$1" override="${2:-}"
	COMPOSE_FILES=(-f docker-compose.yml)
	if [[ -n "$override" ]]; then
		COMPOSE_FILES+=(-f "$override")
	fi

	log "bringing stack down for a clean start"
	docker compose "${COMPOSE_FILES[@]}" down --remove-orphans >/dev/null
	log "starting stack: docker compose ${COMPOSE_FILES[*]} up -d --build --wait --scale limigo=$scale"
	docker compose "${COMPOSE_FILES[@]}" up -d --build --wait --scale "limigo=$scale" >/dev/null

	PROJECT="$(docker compose "${COMPOSE_FILES[@]}" config --format json | jq -r '.name')"
	NETWORK="${PROJECT}_default"

	local cid
	for cid in $(docker compose "${COMPOSE_FILES[@]}" ps -q limigo); do
		docker update --cpuset-cpus="$LIMIGO_CPUSET" "$cid" >/dev/null
	done
	log "pinned $(docker compose "${COMPOSE_FILES[@]}" ps -q limigo | wc -l | tr -d ' ') limigo replica(s) to CPUs $LIMIGO_CPUSET; generator uses $GEN_CPUSET"

	# Docker's healthcheck convergence (--wait) is not the same signal as
	# Traefik's own dynamic routing catching up to the replica set: the first
	# attack fired immediately after a fresh bring_up was observed losing
	# >50% of requests to what vegeta counts as failures (non-2xx), settling
	# to 100% success on every subsequent attack against the same stack.
	# Traefik's docker-provider poll interval and LB healthcheck interval are
	# both on the order of a few seconds, so wait that out here rather than
	# let it corrupt the first row of whichever table runs next.
	log "settling: waiting for Traefik's routing table to catch up to the new replica set"
	sleep 5
}

# run_container_attack <name> <targets-file> [format] — closed-loop,
# max-throughput. Read req/s off this; never latency.
run_container_attack() {
	local name="$1" targets_file="$2" format="${3:-http}"
	log "running $name (in-network, cpuset $GEN_CPUSET, max-throughput)"
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$targets_file")" -format="$format" "${THROUGHPUT_FLAGS[@]}" -output="/raw/$name.bin"
}

# run_container_latency_attack <name> <targets-file> <rate> [format] —
# open-model at a fixed arrival rate, uncapped worker pool. Read latency
# percentiles off this; its req/s is just the offered rate played back.
run_container_latency_attack() {
	local name="$1" targets_file="$2" rate="$3" format="${4:-http}"
	local flags
	flags=$(latency_flags "$rate")
	log "running $name (in-network, cpuset $GEN_CPUSET, open-model at $rate req/s)"
	# Word-splitting $flags is intentional: /bin/bash on macOS is 3.2, so a
	# function cannot return an array. Safe because latency_flags emits exactly
	# `-duration=<DURATION> -rate=<rate>`, neither of which can contain
	# whitespace or a glob.
	# shellcheck disable=SC2086
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$targets_file")" -format="$format" $flags -output="/raw/$name.bin"
}

# run_host_attack <name> <targets-file>
run_host_attack() {
	local name="$1" targets_file="$2"
	log "running $name (host-origin, no CPU pinning available on macOS)"
	vegeta attack -targets="$targets_file" "${THROUGHPUT_FLAGS[@]}" -output="$RAW_DIR/$name.bin"
}

# report_throughput_row <label> <raw-file> [ceiling-req/s] — one markdown row
# from a closed-loop run. Carries no latency columns by construction: those
# would be coordinated-omission-contaminated (see THROUGHPUT_FLAGS). When a
# ceiling is given, appends a "cost vs ceiling" column expressing this row's
# throughput as a percentage of it — never a bare absolute.
report_throughput_row() {
	local label="$1" raw="$2" ceiling="${3:-}"
	local json
	json="$(vegeta report -type=json "$raw")"
	local requests actual_rate success
	requests="$(jq -r '.requests' <<<"$json")"
	# .rate, not .throughput: vegeta defines throughput as *successful*
	# requests per second, and it counts a 429 as unsuccessful. On the allow
	# path the two are identical (success is 100%), but on the denial path
	# .throughput reports 0 req/s for a run that served twelve thousand
	# denials a second — which is the opposite of what that row exists to
	# show. .rate is requests served per second regardless of verdict; the
	# success column beside it carries the allow/deny split.
	actual_rate="$(jq -r '.rate' <<<"$json")"
	success="$(jq -r '.success * 100' <<<"$json")"
	if [[ -n "$ceiling" ]]; then
		local cost_cell
		cost_cell="$(awk -v row="$actual_rate" -v ceiling="$ceiling" 'BEGIN {
			if (ceiling <= 0) { print "n/a"; exit }
			pct = (row / ceiling) * 100
			cost = 100 - pct
			printf "%.1f%% of ceiling (cost ~%.1f%%)", pct, cost
		}')"
		printf '| %s | %s | %.0f | %.2f%% | %s |\n' \
			"$label" "$requests" "$actual_rate" "$success" "$cost_cell"
	else
		printf '| %s | %s | %.0f | %.2f%% |\n' \
			"$label" "$requests" "$actual_rate" "$success"
	fi
}

# report_latency_row <label> <raw-file> <offered-rate> — one markdown row from
# an open-model run. Shows the offered rate next to the attained rate on
# purpose: if the generator could not keep its schedule the two diverge, and
# the percentiles beside them are then measuring a saturated generator rather
# than the service.
report_latency_row() {
	local label="$1" raw="$2" offered="$3"
	local json
	json="$(vegeta report -type=json "$raw")"
	local actual_rate success p50 p95 p99 max
	# .rate, not .throughput, for the same reason report_throughput_row uses
	# it: .throughput counts only 2xx, so any run with denials in it would
	# report an attained rate below the offered rate and trip the
	# "generator fell behind" reading this column exists to give.
	actual_rate="$(jq -r '.rate' <<<"$json")"
	success="$(jq -r '.success * 100' <<<"$json")"
	p50="$(jq -r '.latencies.["50th"] / 1e6' <<<"$json")"
	p95="$(jq -r '.latencies.["95th"] / 1e6' <<<"$json")"
	p99="$(jq -r '.latencies.["99th"] / 1e6' <<<"$json")"
	max="$(jq -r '.latencies.max / 1e6' <<<"$json")"
	printf '| %s | %s | %.0f | %.2f%% | %.2f | %.2f | %.2f | %.2f |\n' \
		"$label" "$offered" "$actual_rate" "$success" "$p50" "$p95" "$p99" "$max"
}

compute_cpusets

# --- group 1: control rows, fresh for this results file -------------------
log "=== control rows ==="
bring_up 1

LIMIGO_CID="$(docker compose ps -q limigo | head -n1)"
REPLICA_IP="$(docker inspect -f "{{ (index .NetworkSettings.Networks \"$NETWORK\").IPAddress }}" "$LIMIGO_CID")"
echo "GET http://$REPLICA_IP:8080/healthz" >"$RAW_DIR/control-direct.http"
cp "$BENCH_DIR/targets/proxy.http" "$RAW_DIR/control-proxy.http"
cp "$BENCH_DIR/targets/host.http" "$RAW_DIR/control-host.http"

run_container_attack control-direct "$RAW_DIR/control-direct.http"
run_container_attack control-proxy "$RAW_DIR/control-proxy.http"
run_host_attack control-host "$RAW_DIR/control-host.http"

# .rate, matching what the control table prints for this same run — the two are
# identical here (a /healthz control run is 100% success by construction), but
# reading a different field than the published row would be a trap for anyone
# who later points this at a path that can return non-2xx.
CEILING_RATE="$(jq -r '.rate' <<<"$(vegeta report -type=json "$RAW_DIR/control-proxy.bin")")"

# Every open-model latency run below is offered this rate: a fixed fraction of
# the measured ceiling, so the generator has headroom to keep its schedule
# through a stall rather than saturating and silently degrading into the
# closed-loop behaviour the open model exists to avoid. 70% is the fraction;
# the ceiling it is 70% of is measured on this box, this run.
LATENCY_RATE="$(awk -v c="$CEILING_RATE" 'BEGIN { printf "%d", c * 0.70 }')"
log "open-model latency runs will be offered $LATENCY_RATE req/s (70% of the measured $(printf '%.0f' "$CEILING_RATE") req/s ceiling)"

run_container_latency_attack control-proxy-latency "$RAW_DIR/control-proxy.http" "$LATENCY_RATE"

# --- group 1b: Redis-side bound (redis-benchmark evalsha) -------------------
#
# The control rows above bound the *transport*: what the generator, Traefik and
# a static 200 can do with no rule logic. This bounds the other end — what
# Redis alone can do with Limigo's actual token_bucket Lua script, measured by
# Redis's own tool, with no Go, no HTTP and no JSON in the path. Every uncached
# allow-path request must wait for exactly this script to run, so no uncached
# row anywhere in this file can exceed this number. It is what turns "85% of
# the transport ceiling" into an attributable statement: the missing cost is
# either Limigo's or Redis's, and this row says which.
log "=== Redis-side bound (redis-benchmark evalsha) ==="
REDIS_IMAGE="$(docker compose config --format json | jq -r '.services.redis.image')"

# SCRIPT LOAD the same file the binary embeds, so the SHA benchmarked is the
# SHA Limigo runs. -x takes the script body from stdin, which avoids quoting a
# multi-line Lua program through two layers of shell.
TB_SHA="$(docker run --rm -i --network "$NETWORK" "$REDIS_IMAGE" \
	redis-cli -h redis -x script load <"$ROOT/internal/store/lua/token_bucket.lua" | tr -d '\r')"
log "loaded token_bucket.lua into the bench Redis as $TB_SHA"

# -r 10000 expands __rand_int__ across a 10k-value space, matching the 10k-key
# cardinality of the algorithm and node axes. The two ARGV values are capacity
# and refill rate, both set far above what the run can consume so the script
# stays on its allow path — the same headroom principle as
# bench/config.loadtest.yaml. -P 1 leaves pipelining off, because Limigo issues
# one EVALSHA per request and does not pipeline. Pinned to GEN_CPUSET so it
# competes with Redis on the same terms the vegeta generator does.
REDIS_BENCH_N="100000"
REDIS_BENCH_C="50"
docker run --rm \
	--network "$NETWORK" \
	--cpuset-cpus="$GEN_CPUSET" \
	"$REDIS_IMAGE" \
	redis-benchmark -h redis -n "$REDIS_BENCH_N" -c "$REDIS_BENCH_C" -P 1 -r 10000 --csv \
	evalsha "$TB_SHA" 1 "limigo:bench:__rand_int__" 1000000 1000000 \
	>"$RAW_DIR/redis-evalsha.csv"

# --csv emits a header line then one data row: "test","rps","avg","min","p50","p95","p99","max".
REDIS_EVALSHA_RPS="$(awk -F'","' 'NR==2 { gsub(/"/, "", $2); print $2 }' "$RAW_DIR/redis-evalsha.csv")"
REDIS_EVALSHA_P99="$(awk -F'","' 'NR==2 { gsub(/"/, "", $7); print $7 }' "$RAW_DIR/redis-evalsha.csv")"
# Older redis-benchmark emits a two-column --csv ("test","rps") with no latency
# percentiles, which would silently leave a blank cell in the results table
# rather than failing. Say so instead.
: "${REDIS_EVALSHA_RPS:?redis-benchmark --csv produced no rps field; check $RAW_DIR/redis-evalsha.csv}"
: "${REDIS_EVALSHA_P99:=n/a (this redis-benchmark reports no percentiles)}"
log "redis-benchmark evalsha: $REDIS_EVALSHA_RPS req/s, p99 ${REDIS_EVALSHA_P99}ms"

# --- group 2: algorithm axis, allow path, 1 replica ------------------------
log "=== algorithm axis (allow path) ==="
bring_up 1 "$BENCH_DIR/docker-compose.bench.yml"

ALGOS=(fixed_window sliding_window token_bucket leaky_bucket)
CARDINALITIES=(1 10000)
PROXY_URL="http://traefik/v1/check"

# Plain indexed arrays, not associative — /bin/bash on macOS is 3.2 (no
# `declare -A`), and this project's other scripts run under that same shell.
ALGO_ROWS=()
ALGO_LATENCY_ROWS=()
for algo in "${ALGOS[@]}"; do
	for card in "${CARDINALITIES[@]}"; do
		name="allow-${algo}-${card}key"
		targets="$RAW_DIR/$name.jsonl"
		bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "loadtest-$algo" "$card" "$targets"
		run_container_attack "$name" "$targets" json
		ALGO_ROWS+=("$(report_throughput_row "$algo, ${card} key(s)" "$RAW_DIR/$name.bin" "$CEILING_RATE")")
	done

	# Latency companion, 10k keys only: the algorithm axis is the one whose
	# percentiles get published, and 10k keys is the cardinality that
	# represents realistic keyspace behaviour rather than single-bucket
	# contention. Reusing the 10k-key targets file the throughput leg just
	# generated.
	lat_name="latency-${algo}-10000key"
	run_container_latency_attack "$lat_name" "$RAW_DIR/allow-${algo}-10000key.jsonl" "$LATENCY_RATE" json
	ALGO_LATENCY_ROWS+=("$(report_latency_row "$algo, 10000 key(s)" "$RAW_DIR/$lat_name.bin" "$LATENCY_RATE")")
done

# --- group 3: denial axis, labelled separately ------------------------------
log "=== denial axis ==="
DENY_NAME="deny-fixed_window-1key"
DENY_TARGETS="$RAW_DIR/$DENY_NAME.jsonl"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "loadtest-deny" 1 "$DENY_TARGETS"
run_container_attack "$DENY_NAME" "$DENY_TARGETS" json
DENY_ROW="$(report_throughput_row "fixed_window (limit 1/60s), 1 key" "$RAW_DIR/$DENY_NAME.bin" "$CEILING_RATE")"

# --- group 4: node axis / scaling curve ------------------------------------
log "=== node axis (token_bucket, 10k keys, allow path) ==="
NODE_ROWS=()
for n in 1 2 3; do
	bring_up "$n" "$BENCH_DIR/docker-compose.bench.yml"
	name="node-$n-token_bucket-10kkey"
	targets="$RAW_DIR/$name.jsonl"
	bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "loadtest-token_bucket" 10000 "$targets"
	run_container_attack "$name" "$targets" json
	NODE_ROWS+=("$(report_throughput_row "$n replica(s)" "$RAW_DIR/$name.bin" "$CEILING_RATE")")
done

# --- group 5: cached vs uncached (config.example.yaml pair) ----------------
log "=== cached vs uncached (fixed sustained rate, allow path) ==="
bring_up 1

# This group was already open-model — a fixed 150 req/s, not -rate=0 — because
# both arms have to stay on the allow path under a shared 200/s refill. It kept
# a -max-workers cap it did not need, which is the one thing that could have
# reintroduced coordinated omission into the only latency numbers here that
# were otherwise sound; the cap is gone, so this group now uses exactly the
# same open model as the latency runs above.
CACHED_PAIR_RATE="150"

UNCACHED_TARGETS="$RAW_DIR/uncached-burst-token_bucket.jsonl"
CACHED_TARGETS="$RAW_DIR/cached-token_bucket.jsonl"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "burst" 1 "$UNCACHED_TARGETS"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "cached" 1 "$CACHED_TARGETS"
run_container_latency_attack uncached-burst-token_bucket "$UNCACHED_TARGETS" "$CACHED_PAIR_RATE" json
run_container_latency_attack cached-token_bucket "$CACHED_TARGETS" "$CACHED_PAIR_RATE" json
UNCACHED_ROW="$(report_latency_row "uncached (burst-tier-token-bucket)" "$RAW_DIR/uncached-burst-token_bucket.bin" "$CACHED_PAIR_RATE")"
CACHED_ROW="$(report_latency_row "cached (cached-tier-token-bucket, local_cache: true)" "$RAW_DIR/cached-token_bucket.bin" "$CACHED_PAIR_RATE")"

# --- write results file -----------------------------------------------------
{
	echo "# Throughput measurement suite — $STAMP"
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
		echo "bench/run-throughput.sh ${INVOCATION[*]}"
	else
		echo "bench/run-throughput.sh"
	fi
	echo '```'
	echo
	echo "CPU pinning: limigo replica(s) on cores $LIMIGO_CPUSET; in-network generator on"
	echo "cores $GEN_CPUSET. The control rows' host-origin leg is **not** CPU-pinned —"
	echo "macOS has no per-process CPU affinity API (see bench/run.sh)."
	echo
	echo "## Measurement model — why throughput and latency come from different runs"
	echo
	echo "**No table in this file reports throughput and latency from the same run.**"
	echo
	echo "Throughput rows are closed-loop: \`-rate=0 -max-workers=$MAX_WORKERS\`, meaning"
	echo "each of $MAX_WORKERS workers sends a request, waits for the response, then sends"
	echo "the next. That measures max sustainable req/s correctly, and measures latency"
	echo "badly. If the service stalls, every worker is parked waiting, so the requests"
	echo "that were due to be sent during the stall are never sent and never timed — the"
	echo "worst moments delete their own evidence, and the reported p99 improves because"
	echo "things got worse. Gil Tene named this **coordinated omission**. vegeta is"
	echo "normally resistant to it (it timestamps at actual send time and grows its worker"
	echo "pool to catch back up), but a fixed \`-max-workers\` cap removes the pool growth"
	echo "that resistance depends on."
	echo
	echo "Latency rows are therefore open-model: a **fixed arrival rate** with **no"
	echo "\`-max-workers\` cap**, so vegeta keeps its schedule through a stall instead of"
	echo "coordinating with it. The rate is $LATENCY_RATE req/s — 70% of the"
	echo "through-Traefik ceiling measured in this same run — leaving the generator"
	echo "headroom to catch up. Each latency table prints the offered rate beside the"
	echo "attained rate: if they diverge, the generator failed to keep schedule and the"
	echo "percentiles beside them describe a saturated generator, not the service."
	echo
	echo "## Control rows"
	echo
	echo "GET /healthz, no rule logic, static 200 (CONTEXT.md, control run). Every table"
	echo "below opens with these and reports limiter throughput as a cost relative to row 2"
	echo "(through-Traefik, in-network) — the ceiling the algorithm and node axes actually"
	echo "run against, since they too go through Traefik."
	echo
	echo "| row | requests | req/s | success |"
	echo "|---|---|---|---|"
	report_throughput_row "1. direct-to-replica, in-network" "$RAW_DIR/control-direct.bin"
	report_throughput_row "2. through-Traefik, in-network (ceiling used below)" "$RAW_DIR/control-proxy.bin"
	report_throughput_row "3. host-origin (through Traefik)" "$RAW_DIR/control-host.bin"
	echo
	echo "The same control path, measured open-model for its latency:"
	echo
	echo "| row | offered req/s | attained req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |"
	echo "|---|---|---|---|---|---|---|---|"
	report_latency_row "through-Traefik, in-network" "$RAW_DIR/control-proxy-latency.bin" "$LATENCY_RATE"
	echo
	echo "### Redis-side bound — \`redis-benchmark evalsha\`"
	echo
	echo "The control rows above bound the **transport**: generator, Traefik and a static"
	echo "200, with no rule logic. This bounds the **other end** — what Redis alone can do"
	echo "with Limigo's own \`token_bucket.lua\`, driven by Redis's own benchmark tool with"
	echo "no Go, no HTTP and no JSON in the path. Every uncached allow-path request must"
	echo "wait for exactly this script, so no uncached row in this file can exceed it."
	echo
	echo "| bound | req/s | p99 (ms) |"
	echo "|---|---|---|"
	echo "| \`redis-benchmark evalsha\` (\`token_bucket.lua\`, SHA \`$TB_SHA\`) | $REDIS_EVALSHA_RPS | $REDIS_EVALSHA_P99 |"
	echo
	echo "Run as \`redis-benchmark -h redis -n $REDIS_BENCH_N -c $REDIS_BENCH_C -P 1 -r 10000 evalsha <sha> 1"
	echo "limigo:bench:__rand_int__ 1000000 1000000\`, in-network and pinned to the same"
	echo "cores as the vegeta generator. The script is \`SCRIPT LOAD\`ed from the same file"
	echo "\`internal/store/lua\` embeds, so the SHA benchmarked is the SHA Limigo runs."
	echo "\`-r 10000\` expands \`__rand_int__\` over a 10k-value keyspace to match the"
	echo "cardinality of the algorithm and node axes; the two ARGV values are capacity and"
	echo "refill rate, set high enough that the script stays on its allow path. \`-P 1\`"
	echo "leaves pipelining off because Limigo issues one \`EVALSHA\` per request and does"
	echo "not pipeline."
	echo
	echo "Two caveats this row does not hide: \`redis-benchmark\` drives its own $REDIS_BENCH_C"
	echo "connections rather than replaying Limigo's concurrency, and it is co-resident"
	echo "with Redis in the same Docker VM as everything else here. It is an upper bound"
	echo "on the Redis leg under this tool's load, not a claim about Redis in general."
	echo
	echo "## Algorithm axis — allow path, uncached, 1 replica"
	echo
	echo "POST /v1/check through Traefik, in-network, rules from \`bench/config.loadtest.yaml\`"
	echo "(headroom limits — allow path stays hot). 1 key measures single-bucket contention;"
	echo "10k keys measures map growth and Redis keyspace behaviour. Never blended."
	echo
	echo "| row | requests | req/s | success | cost vs ceiling |"
	echo "|---|---|---|---|---|"
	for row in "${ALGO_ROWS[@]}"; do
		echo "$row"
	done
	echo
	echo "### Algorithm axis — latency, open-model"
	echo
	echo "The same four algorithms at 10k keys, re-run open-model at $LATENCY_RATE req/s"
	echo "(70% of the measured ceiling, uncapped worker pool) because the percentiles from"
	echo "the closed-loop table above would be coordinated-omission-contaminated. 10k keys"
	echo "only: that is the cardinality representing realistic keyspace behaviour rather"
	echo "than single-bucket contention."
	echo
	echo "| row | offered req/s | attained req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |"
	echo "|---|---|---|---|---|---|---|---|"
	for row in "${ALGO_LATENCY_ROWS[@]}"; do
		echo "$row"
	done
	echo
	echo "## Denial path — labelled separately, never blended with the allow-path rows above"
	echo
	echo "Same shape of request, matched instead by \`load-test-deny\` (limit 1 per 60s), so"
	echo "every request past the first is a denial. Denials are cheaper (CONTEXT.md,"
	echo "allow path vs denial path); this row exists to show that, not to represent"
	echo "achievable allow-path throughput."
	echo
	echo "| row | requests | req/s | success | cost vs ceiling |"
	echo "|---|---|---|---|---|"
	echo "$DENY_ROW"
	echo
	echo "## Node axis — scaling curve"
	echo
	echo "token_bucket, 10k keys, allow path, through Traefik. The claim under test is"
	echo "approximately linear throughput growth with node count — not any particular"
	echo "absolute number. Cost is reported against the single-replica through-Traefik"
	echo "ceiling from the control rows above: that ceiling is a property of the"
	echo "generator/Traefik/network path, not of how many limigo replicas sit behind it,"
	echo "so it does not need re-measuring per node count — a row exceeding 100% here"
	echo "would itself be a finding (Traefik load-balancing outrunning a single-replica"
	echo "baseline), not an error."
	echo
	echo "| row | requests | req/s | success | cost vs ceiling |"
	echo "|---|---|---|---|---|"
	for row in "${NODE_ROWS[@]}"; do
		echo "$row"
	done
	echo
	echo "## Cached vs uncached — controlled comparison (config.example.yaml pair)"
	echo
	echo "\`burst-tier-token-bucket\` (capacity 1000, rate 200, no cache) vs"
	echo "\`cached-tier-token-bucket\` (identical parameters, \`local_cache: true\`)."
	echo "Both held to a fixed **$CACHED_PAIR_RATE req/s** — under the shared 200/s refill rate — so"
	echo "the allow path stays hot for both arms instead of collapsing into the denial"
	echo "path once the 1000-token burst capacity drains. At this rate the comparison is"
	echo "latency (does the local cache avoid a Redis round-trip), not max throughput —"
	echo "measuring throughput at these low, realistic limits would mostly measure how"
	echo "fast Limigo can say no once capacity is exhausted, which is what"
	echo "bench/run-overshoot.sh measures, not this harness."
	echo
	echo "Both arms are open-model — fixed arrival rate, uncapped worker pool — so these"
	echo "percentiles carry the same coordinated-omission guarantee as the latency tables"
	echo "above. Both are offered the same 150 req/s, so the attained-rate column is a"
	echo "check that neither arm fell behind, not a result; the comparison is the"
	echo "percentiles to its right."
	echo
	echo "| row | offered req/s | attained req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |"
	echo "|---|---|---|---|---|---|---|---|"
	echo "$UNCACHED_ROW"
	echo "$CACHED_ROW"
	echo
	echo "Raw vegeta output (binary attack results, generated target files, and per-run"
	echo "target JSONL) is kept under \`bench/results/raw/$STAMP-throughput/\` (gitignored —"
	echo "regenerate by rerunning this script rather than diffing binary blobs)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"

if [[ "$KEEP_STACK" != "1" ]]; then
	log "bringing stack down"
	docker compose down --remove-orphans >/dev/null
fi
