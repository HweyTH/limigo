#!/usr/bin/env bash
#
# bench/run-demo.sh — the one-node vs many-node demonstration.
#
# Not a measurement. This script drives the stack through a scripted scene
# for the README's demo recording: it offers a fixed request rate to one
# rule whose limit is below that rate, first against a single limigo
# replica, then against several — and leaves the stack up so the Grafana
# dashboard can be watched or recorded. The thing to look at:
#
#   "Request rate by outcome"  — allowed stays pinned at the rule's refill
#                                rate no matter how many nodes serve it;
#                                the excess is denied. That is the atomic
#                                Lua counter doing its job.
#   "Request rate by node"     — one line becomes N lines, each carrying
#                                ~1/N of the offered load.
#
# The rule is config.example.yaml's burst-tier-token-bucket (rate 200/s,
# capacity 1000), the image's baked-in config, so no override is needed.
# With 300 req/s offered against a 200/s refill, allowed settles at 200/s
# and denied at 100/s. Each phase starts from a full bucket (fresh Redis for
# phase 1; the between-phase pause refills it before phase 2), so both phases
# open with the same ~10s burst of extra admits before settling — the burst
# allowance is the algorithm's feature, and it is deliberately shown twice
# so the two phases are comparable.
#
# Usage: bench/run-demo.sh [--rate 300] [--phase-duration 60s] [--scale-to 3]
#
# The stack is left running on purpose. Tear it down afterwards with
# `docker compose down`.
#
# Unlike the run-*.sh measurement scripts this one needs only docker on the
# host: no jq, no vegeta, no CPU pinning (300 req/s is far below any
# contention that pinning exists to remove). Targets go to the generator over
# stdin and its output comes back the same way, so nothing is bind-mounted.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

RATE="300"
PHASE_DURATION="60s"
SCALE_TO="3"

while [[ $# -gt 0 ]]; do
	case "$1" in
	--rate)
		RATE="$2"
		shift 2
		;;
	--phase-duration)
		PHASE_DURATION="$2"
		shift 2
		;;
	--scale-to)
		SCALE_TO="$2"
		shift 2
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
if ((SCALE_TO < 2)); then
	echo "--scale-to ($SCALE_TO) must be at least 2, or there is no many-node phase to show" >&2
	exit 1
fi

# The rule under demonstration (config.example.yaml, burst-tier-token-bucket).
# Hardcoded for the same reason bench/run-overshoot.sh hardcodes its pair: the
# numbers printed below must match that file, so if it changes, change this.
PLAN="burst"
REFILL_RATE=200
CAPACITY=1000

if ((RATE <= REFILL_RATE)); then
	echo "--rate ($RATE) must exceed the rule's refill rate ($REFILL_RATE), or nothing is ever denied and there is nothing to show" >&2
	exit 1
fi

log() { echo "[demo] $*" >&2; }

# One key, one target line, in vegeta's JSON format (its http format can only
# carry a body via @file, which would need a bind mount). The body is base64
# per that format's spec; `tr -d '\n'` guards against GNU base64's line
# wrapping, which macOS's base64 doesn't do.
BODY_B64="$(printf '%s' '{"key":"demo"}' | base64 | tr -d '\n')"
TARGET_JSON="$(printf '{"method":"POST","url":"http://traefik/v1/check","header":{"Content-Type":["application/json"],"X-Plan":["%s"]},"body":"%s"}' "$PLAN" "$BODY_B64")"

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# attack <label> — open-model at the offered rate for one phase, in-network
# through Traefik so replicas are load-balanced exactly as a client sees
# them. Prints vegeta's text report: its "Status Codes" line is the textual
# version of what the dashboard shows (200 = allowed, 429 = denied).
attack() {
	local label="$1"
	log "$label: offering $RATE req/s to rule '$PLAN' for $PHASE_DURATION"
	printf '%s\n' "$TARGET_JSON" |
		docker run --rm -i --network "$NETWORK" limigo-bench-vegeta \
			attack -format=json -rate="$RATE" -duration="$PHASE_DURATION" |
		docker run --rm -i limigo-bench-vegeta report
}

# --- phase 1: one node ------------------------------------------------------

log "bringing stack down for a clean start (fresh Redis, full bucket)"
docker compose down --remove-orphans >/dev/null
log "starting stack: docker compose up -d --build --wait --scale limigo=1"
docker compose up -d --build --wait --scale limigo=1 >/dev/null

# The compose network, read off a running replica rather than derived from
# the project name (which depends on the checkout directory's name).
NETWORK="$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{end}}' "$(docker compose ps -q limigo | head -n 1)")"

log "dashboard: http://localhost:3000/d/limigo  (Prometheus targets: http://localhost:9090/targets)"
log "settling: Traefik's routing table and Prometheus's docker_sd both lag the replica set by a few seconds"
sleep 30

attack "phase 1 (1 node)"

# --- phase 2: many nodes ----------------------------------------------------

log "scaling: docker compose up -d --wait --scale limigo=$SCALE_TO (existing replica kept, Redis state kept)"
docker compose up -d --wait --scale "limigo=$SCALE_TO" >/dev/null

# Prometheus's docker_sd refreshes every 15s (prometheus/prometheus.yml), then
# needs a scrape, then a second one before rate() has anything to say — and
# in practice the new replicas were seen 30–45s after the scale-up. Traefik
# picks them up in a few seconds. Waiting this out means phase 2's first
# samples already show every node, rather than lines appearing mid-phase.
log "settling: waiting for Prometheus docker_sd (15s refresh) and Traefik to discover the new replicas"
sleep 45

attack "phase 2 ($SCALE_TO nodes)"

log "done. Expected on the dashboard: allowed ≈ $REFILL_RATE req/s in both phases (after a ~$((CAPACITY / (RATE - REFILL_RATE)))s burst), denied ≈ $((RATE - REFILL_RATE)) req/s; 'by node' splits from 1 to $SCALE_TO lines."
log "the stack is still running — tear it down with: docker compose down"
