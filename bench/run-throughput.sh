#!/usr/bin/env bash
#
# bench/run-throughput.sh — throughput/latency measurement suite (ticket 07).
#
# Produces the algorithm comparison table and the scaling curve, both opening
# with a fresh set of the three control rows from ticket 06 (ADR-0004) so every
# limiter number in this file can be read as a cost relative to the ceiling,
# not a bare absolute.
#
# Five run groups, each against POST /v1/check through Traefik (in-network —
# the primary number per CONTEXT.md), except group 1 which hits /healthz:
#
#   1. Control rows       — GET /healthz, 1 replica, no rule logic (ADR-0004).
#   2. Algorithm axis      — all four algorithms, allow path, 1 replica, at
#                            both 1 key and 10k keys (CONTEXT.md, cardinality).
#   3. Denial axis         — the load-test-deny rule, 1 replica, labelled
#                            separately from the allow path (CONTEXT.md).
#   4. Node axis           — token_bucket, 10k keys, allow path, at 1/2/3
#                            replicas (ADR-0002, scaling curve).
#   5. Cached vs uncached  — config.example.yaml's burst-tier/cached-tier
#                            token-bucket pair, held to a fixed rate under
#                            their shared 200/s refill so the allow path stays
#                            hot for both arms; the comparison is latency
#                            (fewer Redis round-trips), not max throughput.
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

ATTACK_FLAGS=(-duration="$DURATION" -rate=0 -max-workers="$MAX_WORKERS")

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

# run_container_attack <name> <targets-file>
run_container_attack() {
	local name="$1" targets_file="$2" format="${3:-http}"
	log "running $name (in-network, cpuset $GEN_CPUSET)"
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$targets_file")" -format="$format" "${ATTACK_FLAGS[@]}" -output="/raw/$name.bin"
}

# run_host_attack <name> <targets-file>
run_host_attack() {
	local name="$1" targets_file="$2"
	log "running $name (host-origin, no CPU pinning available on macOS)"
	vegeta attack -targets="$targets_file" "${ATTACK_FLAGS[@]}" -output="$RAW_DIR/$name.bin"
}

# report_row <label> <raw-file> [ceiling-req/s] — one markdown table row. When
# a ceiling is given, appends a "cost vs ceiling" column expressing this row's
# throughput as a percentage of it (ADR-0004: never a bare absolute).
report_row() {
	local label="$1" raw="$2" ceiling="${3:-}"
	local json
	json="$(vegeta report -type=json "$raw")"
	local requests actual_rate success p50 p95 p99 max
	requests="$(jq -r '.requests' <<<"$json")"
	actual_rate="$(jq -r '.throughput' <<<"$json")"
	success="$(jq -r '.success * 100' <<<"$json")"
	p50="$(jq -r '.latencies.["50th"] / 1e6' <<<"$json")"
	p95="$(jq -r '.latencies.["95th"] / 1e6' <<<"$json")"
	p99="$(jq -r '.latencies.["99th"] / 1e6' <<<"$json")"
	max="$(jq -r '.latencies.max / 1e6' <<<"$json")"
	if [[ -n "$ceiling" ]]; then
		local cost_cell
		cost_cell="$(awk -v row="$actual_rate" -v ceiling="$ceiling" 'BEGIN {
			if (ceiling <= 0) { print "n/a"; exit }
			pct = (row / ceiling) * 100
			cost = 100 - pct
			printf "%.1f%% of ceiling (cost ~%.1f%%)", pct, cost
		}')"
		printf '| %s | %s | %.0f | %.2f%% | %.2f | %.2f | %.2f | %.2f | %s |\n' \
			"$label" "$requests" "$actual_rate" "$success" "$p50" "$p95" "$p99" "$max" "$cost_cell"
	else
		printf '| %s | %s | %.0f | %.2f%% | %.2f | %.2f | %.2f | %.2f |\n' \
			"$label" "$requests" "$actual_rate" "$success" "$p50" "$p95" "$p99" "$max"
	fi
}

compute_cpusets

# --- group 1: control rows (ADR-0004), fresh for this results file --------
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

CEILING_RATE="$(jq -r '.throughput' <<<"$(vegeta report -type=json "$RAW_DIR/control-proxy.bin")")"

# --- group 2: algorithm axis, allow path, 1 replica ------------------------
log "=== algorithm axis (allow path) ==="
bring_up 1 "$BENCH_DIR/docker-compose.bench.yml"

ALGOS=(fixed_window sliding_window token_bucket leaky_bucket)
CARDINALITIES=(1 10000)
PROXY_URL="http://traefik/v1/check"

# Plain indexed arrays, not associative — /bin/bash on macOS is 3.2 (no
# `declare -A`), and this project's other scripts run under that same shell.
ALGO_ROWS=()
for algo in "${ALGOS[@]}"; do
	for card in "${CARDINALITIES[@]}"; do
		name="allow-${algo}-${card}key"
		targets="$RAW_DIR/$name.jsonl"
		bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "loadtest-$algo" "$card" "$targets"
		run_container_attack "$name" "$targets" json
		ALGO_ROWS+=("$(report_row "$algo, ${card} key(s)" "$RAW_DIR/$name.bin" "$CEILING_RATE")")
	done
done

# --- group 3: denial axis, labelled separately ------------------------------
log "=== denial axis ==="
DENY_NAME="deny-fixed_window-1key"
DENY_TARGETS="$RAW_DIR/$DENY_NAME.jsonl"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "loadtest-deny" 1 "$DENY_TARGETS"
run_container_attack "$DENY_NAME" "$DENY_TARGETS" json
DENY_ROW="$(report_row "fixed_window (limit 1/60s), 1 key" "$RAW_DIR/$DENY_NAME.bin" "$CEILING_RATE")"

# --- group 4: node axis / scaling curve ------------------------------------
log "=== node axis (token_bucket, 10k keys, allow path) ==="
NODE_ROWS=()
NODE_CEILING="$CEILING_RATE"
for n in 1 2 3; do
	bring_up "$n" "$BENCH_DIR/docker-compose.bench.yml"
	name="node-$n-token_bucket-10kkey"
	targets="$RAW_DIR/$name.jsonl"
	bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "loadtest-token_bucket" 10000 "$targets"
	run_container_attack "$name" "$targets" json
	NODE_ROWS+=("$(report_row "$n replica(s)" "$RAW_DIR/$name.bin" "$NODE_CEILING")")
done

# --- group 5: cached vs uncached (config.example.yaml pair) ----------------
log "=== cached vs uncached (fixed sustained rate, allow path) ==="
bring_up 1
SUSTAINED_FLAGS=(-duration="$DURATION" -rate=150 -max-workers="$MAX_WORKERS")

run_cached_pair() {
	local name="$1" targets_file="$2"
	log "running $name (in-network, cpuset $GEN_CPUSET, fixed rate 150/s)"
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$targets_file")" -format=json "${SUSTAINED_FLAGS[@]}" -output="/raw/$name.bin"
}

UNCACHED_TARGETS="$RAW_DIR/uncached-burst-token_bucket.jsonl"
CACHED_TARGETS="$RAW_DIR/cached-token_bucket.jsonl"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "burst" 1 "$UNCACHED_TARGETS"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "cached" 1 "$CACHED_TARGETS"
run_cached_pair uncached-burst-token_bucket "$UNCACHED_TARGETS"
run_cached_pair cached-token_bucket "$CACHED_TARGETS"
UNCACHED_ROW="$(report_row "uncached (burst-tier-token-bucket)" "$RAW_DIR/uncached-burst-token_bucket.bin" "$CEILING_RATE")"
CACHED_ROW="$(report_row "cached (cached-tier-token-bucket, local_cache: true)" "$RAW_DIR/cached-token_bucket.bin" "$CEILING_RATE")"

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
	echo "## Control rows (ADR-0004)"
	echo
	echo "GET /healthz, no rule logic, static 200 (CONTEXT.md, control run). Every table"
	echo "below opens with these and reports limiter throughput as a cost relative to row 2"
	echo "(through-Traefik, in-network) — the ceiling the algorithm and node axes actually"
	echo "run against, since they too go through Traefik."
	echo
	echo "| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |"
	echo "|---|---|---|---|---|---|---|---|"
	report_row "1. direct-to-replica, in-network" "$RAW_DIR/control-direct.bin"
	report_row "2. through-Traefik, in-network (ceiling used below)" "$RAW_DIR/control-proxy.bin"
	report_row "3. host-origin (through Traefik)" "$RAW_DIR/control-host.bin"
	echo
	echo "## Algorithm axis — allow path, uncached, 1 replica"
	echo
	echo "POST /v1/check through Traefik, in-network, rules from \`bench/config.loadtest.yaml\`"
	echo "(headroom limits — allow path stays hot). 1 key measures single-bucket contention;"
	echo "10k keys measures map growth and Redis keyspace behaviour. Never blended."
	echo
	echo "| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |"
	echo "|---|---|---|---|---|---|---|---|---|"
	for row in "${ALGO_ROWS[@]}"; do
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
	echo "| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |"
	echo "|---|---|---|---|---|---|---|---|---|"
	echo "$DENY_ROW"
	echo
	echo "## Node axis — scaling curve (ADR-0002)"
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
	echo "| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |"
	echo "|---|---|---|---|---|---|---|---|---|"
	for row in "${NODE_ROWS[@]}"; do
		echo "$row"
	done
	echo
	echo "## Cached vs uncached — controlled comparison (config.example.yaml pair)"
	echo
	echo "\`burst-tier-token-bucket\` (capacity 1000, rate 200, no cache) vs"
	echo "\`cached-tier-token-bucket\` (identical parameters, \`local_cache: true\`)."
	echo "Both held to a fixed **150 req/s** — under the shared 200/s refill rate — so"
	echo "the allow path stays hot for both arms instead of collapsing into the denial"
	echo "path once the 1000-token burst capacity drains. At this rate the comparison is"
	echo "latency (does the local cache avoid a Redis round-trip), not max throughput —"
	echo "measuring throughput at these low, realistic limits would mostly measure how"
	echo "fast Limigo can say no once capacity is exhausted, which is ticket 08's"
	echo "overshoot harness, not this one's."
	echo
	echo "Both rows are throttled to the same 150 req/s, so their near-identical, near-0%"
	echo "cost-vs-ceiling figures are expected and not the point of this table — the"
	echo "column is kept for consistency with every other table here (ADR-0004: never a"
	echo "bare absolute). The comparison that matters is the latency columns to its left."
	echo
	echo "| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | cost vs ceiling |"
	echo "|---|---|---|---|---|---|---|---|---|"
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
