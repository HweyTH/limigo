#!/usr/bin/env bash
#
# bench/run.sh — the three control rows (ADR-0004).
#
# Runs a vegeta load test against GET /healthz (no rule logic, CONTEXT.md
# "control run") and writes a dated results file with all three rows:
#
#   1. direct   — in-network, straight to one limigo replica (Go HTTP stack only)
#   2. proxy    — in-network, through Traefik (adds proxy cost)
#   3. host     — from the host, through Traefik (adds the Docker Desktop VM
#                 network boundary on top of the proxy cost — see ADR-0003
#                 consequences: individual replicas have no host port, so
#                 Traefik is the only path reachable from outside the network)
#
# Usage: bench/run.sh [--duration 30s] [--max-workers 200] [--keep-stack]
#
# The generator is CPU-pinned away from the limigo replica for the in-network
# runs (both run as containers in the same Linux VM, so `docker update
# --cpuset-cpus` applies to both). The host-origin run cannot be pinned: this
# script runs on macOS, which has no per-process CPU affinity API comparable
# to Linux cpuset/taskset. That gap is recorded in the results file rather
# than silently ignored.
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
	echo "vegeta not found on PATH — needed for the host-origin run (brew install vegeta)" >&2
	exit 1
}

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG.md"

log() { echo "[bench] $*" >&2; }

# --- clean stack start -------------------------------------------------
log "bringing stack down for a clean start"
docker compose down --remove-orphans >/dev/null

log "building and starting a single limigo replica"
docker compose up -d --build --scale limigo=1 --wait

PROJECT="$(docker compose config --format json | jq -r '.name')"
NETWORK="${PROJECT}_default"

LIMIGO_CID="$(docker compose ps -q limigo | head -n1)"
if [[ -z "$LIMIGO_CID" ]]; then
	echo "could not find a running limigo container" >&2
	exit 1
fi
REPLICA_IP="$(docker inspect -f "{{ (index .NetworkSettings.Networks \"$NETWORK\").IPAddress }}" "$LIMIGO_CID")"
if [[ -z "$REPLICA_IP" ]]; then
	echo "could not determine limigo replica IP on network $NETWORK" >&2
	exit 1
fi
echo "GET http://$REPLICA_IP:8080/healthz" >"$RAW_DIR/direct.http"

# --- CPU pinning: generator away from the limigo replica ---------------
# cpuset pinning applies inside the Docker engine, so the CPU count that
# matters is the one Docker itself reports — not the macOS host's, which on
# Docker Desktop can (and often does) exceed the VM's configured CPU count.
TOTAL_CPUS="$(docker info --format '{{.NCPU}}')"
if ((TOTAL_CPUS < 2)); then
	echo "need at least 2 CPUs available to Docker to pin the generator away from limigo; have $TOTAL_CPUS" >&2
	exit 1
fi
HALF=$((TOTAL_CPUS / 2))
LIMIGO_CPUSET="0-$((HALF - 1))"
GEN_CPUSET="$HALF-$((TOTAL_CPUS - 1))"
log "pinning limigo replica to CPUs $LIMIGO_CPUSET, generator to $GEN_CPUSET"
docker update --cpuset-cpus="$LIMIGO_CPUSET" "$LIMIGO_CID" >/dev/null

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# Shared vegeta attack flags for every leg: -rate=0 means unthrottled (find
# the ceiling, not a fixed offered load), capped by -max-workers.
ATTACK_FLAGS=(-duration="$DURATION" -rate=0 -max-workers="$MAX_WORKERS")

# run_container_attack <name> <target-file>
run_container_attack() {
	local name="$1" target_file="$2"
	log "running $name (in-network, cpuset $GEN_CPUSET)"
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$(basename "$target_file")" "${ATTACK_FLAGS[@]}" -output="/raw/$name.bin"
}

# run_host_attack <name> <target-file>
run_host_attack() {
	local name="$1" target_file="$2"
	log "running $name (host-origin, no CPU pinning available on macOS)"
	vegeta attack -targets="$target_file" "${ATTACK_FLAGS[@]}" -output="$RAW_DIR/$name.bin"
}

cp "$BENCH_DIR/targets/proxy.http" "$RAW_DIR/proxy.http"
cp "$BENCH_DIR/targets/host.http" "$RAW_DIR/host.http"

run_container_attack direct "$RAW_DIR/direct.http"
run_container_attack proxy "$RAW_DIR/proxy.http"
run_host_attack host "$RAW_DIR/host.http"

# report_row <name> <raw-file>
report_row() {
	local name="$1" raw="$2"
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
	printf '| %s | %s | %.0f | %.2f%% | %.2f | %.2f | %.2f | %.2f |\n' \
		"$name" "$requests" "$actual_rate" "$success" "$p50" "$p95" "$p99" "$max"
}

# --- write results file --------------------------------------------------
{
	echo "# Control run — $STAMP"
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
		echo "bench/run.sh ${INVOCATION[*]}"
	else
		echo "bench/run.sh"
	fi
	echo '```'
	echo
	echo "CPU pinning: limigo replica on cores $LIMIGO_CPUSET; in-network generator on"
	echo "cores $GEN_CPUSET. The host-origin row is **not** CPU-pinned — macOS has no"
	echo "per-process CPU affinity API, so this is a known gap rather than an omission."
	echo
	echo "## Control rows (ADR-0004)"
	echo
	echo "All three rows hit \`GET /healthz\` — no rule logic, static 200 (CONTEXT.md,"
	echo "control run). Rate is unbounded (\`-rate=0\`, capped by \`-max-workers=$MAX_WORKERS\`),"
	echo "so throughput is the ceiling this hardware and this path can sustain."
	echo
	echo "| row | requests | req/s | success | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) |"
	echo "|---|---|---|---|---|---|---|---|"
	report_row "1. direct-to-replica, in-network" "$RAW_DIR/direct.bin"
	report_row "2. through-Traefik, in-network" "$RAW_DIR/proxy.bin"
	report_row "3. host-origin (through Traefik)" "$RAW_DIR/host.bin"
	echo
	echo "Raw vegeta output (binary attack results plus the target files used) is kept"
	echo "under \`bench/results/raw/$STAMP/\` (gitignored — regenerate by rerunning this"
	echo "script rather than diffing binary blobs)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"

if [[ "$KEEP_STACK" != "1" ]]; then
	log "bringing stack down"
	docker compose down --remove-orphans >/dev/null
fi
