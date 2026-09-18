#!/usr/bin/env bash
#
# bench/run-rolling-restart.sh — rolling restart under load.
#
# The claim under test: restarting Limigo replicas one at a time behind
# Traefik drops no requests. That depends on the readiness split — on
# SIGTERM a replica fails GET /readyz first, keeps serving for --drain-delay
# so Traefik's next health check routes new work to its peers, and only then
# closes its listener and drains what is in flight. Before that split,
# Traefik kept routing to a replica right up to the moment it stopped
# listening, and every request in that gap failed.
#
# A steady, under-limit stream is offered to the stack for the whole run;
# partway through, each replica is restarted in turn (SIGTERM, wait for it
# to exit, start it again, wait for it to be healthy and back in Traefik's
# pool) before the next one is touched. Every response is bucketed per
# second from vegeta's own timestamps, so the timeline shows exactly what
# happened around each restart. The offered rate is below the rule's refill
# rate, so with the stack healthy every request is a 200: anything else in
# the table is the restart, not the limit.
#
# "Dropped" here means a request that did not get a 200: a 502/503/504 from
# Traefik or a replica, or no HTTP response at all (a reset, code 0 in
# vegeta's terms). A 429 would mean the limit was hit, which the rate is
# chosen to make impossible; if one shows up, the run is mislabelled, not
# the restart.
#
# Per CONTEXT.md this is a measurement, not a claim: whatever the timeline
# shows is published as-is.
#
# Usage: bench/run-rolling-restart.sh [--rate 100] [--duration 130] [--first-restart-at 15] [--scale 3] [--keep-stack]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

RATE="100"
DURATION="130"
FIRST_RESTART_AT="15"
SCALE="3"
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
	--first-restart-at)
		FIRST_RESTART_AT="$2"
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

if ((SCALE < 2)); then
	echo "--scale ($SCALE) must be at least 2: a rolling restart needs a peer to route to" >&2
	exit 1
fi

# burst-tier-token-bucket (config.example.yaml): refill 200/s. Hardcoded
# because the reading of the timeline depends on it.
REFILL_RATE=200
if ((RATE >= REFILL_RATE)); then
	echo "--rate ($RATE) must be below the rule's refill rate ($REFILL_RATE/s) so a healthy stack admits every request and only the restarts show in the timeline" >&2
	exit 1
fi

# Each restart is bounded by the stop grace period (20s, docker-compose.yml)
# plus the time to come back healthy and be re-added by Traefik; budget 35s
# per replica and refuse a duration that cannot fit them all with a healthy
# tail to prove recovery.
PER_RESTART_BUDGET=35
if ((DURATION < FIRST_RESTART_AT + SCALE * PER_RESTART_BUDGET + 10)); then
	echo "--duration ($DURATION) is too short for $SCALE restarts starting at t+${FIRST_RESTART_AT}s; need at least $((FIRST_RESTART_AT + SCALE * PER_RESTART_BUDGET + 10))s" >&2
	exit 1
fi

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP-rolling-restart"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG-rolling-restart.md"

log() { echo "[bench-rolling-restart] $*" >&2; }

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# compute_cpusets and bring_up are the same as bench/run-overshoot.sh's; see
# the notes there.
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
now_epoch() { date +%s; }

# wait_healthy <cid> — blocks until Docker's liveness check on the container
# passes, then one Traefik check interval more so the replica is back in the
# routing pool before the next one is taken out. Without that second wait
# two replicas could be out at once, which is not a rolling restart.
wait_healthy() {
	local cid="$1" status
	for _ in $(seq 1 60); do
		status="$(docker inspect --format '{{.State.Health.Status}}' "$cid")"
		if [[ "$status" == "healthy" ]]; then
			sleep 6
			return 0
		fi
		sleep 1
	done
	echo "FATAL: replica $cid did not become healthy within 60s" >&2
	exit 1
}

TARGETS="$RAW_DIR/rolling.jsonl"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "burst" 1 "$TARGETS"

bring_up "$SCALE"

log "starting generator: ${RATE}/1s for ${DURATION}s, single key"
# The targets file is copied into the container rather than read through the
# bind mount: on Docker Desktop, reading a bind-mounted file while sibling
# containers are being restarted has failed with EDEADLK ("resource deadlock
# avoided") and taken the whole run with it. Output still goes to the mount.
CONTAINER="$(docker create \
	--network "$NETWORK" \
	--cpuset-cpus="$GEN_CPUSET" \
	-v "$RAW_DIR:/raw" \
	limigo-bench-vegeta \
	attack -targets=/targets.jsonl -format=json \
	-rate="${RATE}/1s" -duration="${DURATION}s" \
	-output="/raw/rolling.bin")"
docker cp "$TARGETS" "$CONTAINER:/targets.jsonl"
docker start "$CONTAINER" >/dev/null
T0_HOST="$(now_epoch)"

sleep "$FIRST_RESTART_AT"

# One restart per replica, strictly in sequence. docker restart -t must
# exceed the drain delay plus the shutdown timeout, or Docker SIGKILLs the
# replica mid-drain and the measurement is of Docker, not of Limigo.
RESTART_STAMPS=()
for cid in $(docker compose ps -q limigo); do
	name="$(docker inspect --format '{{.Name}}' "$cid" | sed 's|^/||')"
	began="$(now_epoch)"
	log "restarting $name (SIGTERM, up to 20s grace, then start)"
	docker restart -t 20 "$cid" >/dev/null
	restarted="$(now_epoch)"
	wait_healthy "$cid"
	pooled="$(now_epoch)"
	RESTART_STAMPS+=("$name|$began|$restarted|$pooled")
done

log "waiting for the generator to finish"
EXIT_CODE="$(docker wait "$CONTAINER")"
if [[ "$EXIT_CODE" != "0" ]]; then
	echo "FATAL: generator exited $EXIT_CODE; its output follows" >&2
	docker logs "$CONTAINER" >&2 || true
	docker rm "$CONTAINER" >/dev/null
	exit 1
fi
docker rm "$CONTAINER" >/dev/null

# Decode and bucket per second. Columns:
# second|200|429|5xx|other, where 5xx is any 500–599 (a replica's 503, or
# Traefik's 502/504 when it had nowhere to send the request) and other is
# everything else including code 0 (no HTTP response).
docker run --rm -v "$RAW_DIR:/raw" limigo-bench-vegeta encode -to=json "/raw/rolling.bin" >"$RAW_DIR/rolling.jsonl.out"
FIRST_EPOCH="$(jq -rs 'map(.timestamp | sub("\\.[0-9]+"; "") | fromdateiso8601) | min' "$RAW_DIR/rolling.jsonl.out")"
jq -r --argjson t0 "$FIRST_EPOCH" '
	((.timestamp | sub("\\.[0-9]+"; "") | fromdateiso8601) - $t0) as $s
	| "\($s) \(.code)"' "$RAW_DIR/rolling.jsonl.out" |
	awk -v duration="$DURATION" '
		{ if ($2 == 200) a[$1]++; else if ($2 == 429) d[$1]++; else if ($2 >= 500 && $2 <= 599) e[$1]++; else o[$1]++; if ($1 > last) last = $1 }
		END { if (last < duration - 1) last = duration - 1; for (s = 0; s <= last; s++) printf "%d|%d|%d|%d|%d\n", s, a[s] + 0, d[s] + 0, e[s] + 0, o[s] + 0 }' \
		>"$RAW_DIR/rolling.seconds"

TOTALS="$(awk -F'|' '{ a += $2; d += $3; e += $4; o += $5 } END { printf "%d|%d|%d|%d", a, d, e, o }' "$RAW_DIR/rolling.seconds")"
IFS='|' read -r TOTAL_OK TOTAL_DENIED TOTAL_5XX TOTAL_OTHER <<<"$TOTALS"
DROPPED=$((TOTAL_5XX + TOTAL_OTHER))
SENT=$((TOTAL_OK + TOTAL_DENIED + TOTAL_5XX + TOTAL_OTHER))

RUNS="$(awk -F'|' '
	function shape(a, d, e, o) { return (a > 0) "" (d > 0) "" (e > 0) "" (o > 0) }
	function emit() { if (start >= 0) printf "%d–%d|%d|%d|%d|%d\n", start, prev, ra, rd, re, ro }
	BEGIN { start = -1 }
	{
		sh = shape($2, $3, $4, $5)
		if (sh != cur) { emit(); cur = sh; start = $1; ra = rd = re = ro = 0 }
		prev = $1; ra += $2; rd += $3; re += $4; ro += $5
	}
	END { emit() }' "$RAW_DIR/rolling.seconds")"

STATUS_CODES="$(docker run --rm -v "$RAW_DIR:/raw" limigo-bench-vegeta report -type=json "/raw/rolling.bin" | jq -r '.status_codes | to_entries | map("\(.key)=\(.value)") | join(", ")')"

# --- write results file ------------------------------------------------------
{
	echo "# Rolling restart under load — $STAMP"
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
		echo "bench/run-rolling-restart.sh ${INVOCATION[*]}"
	else
		echo "bench/run-rolling-restart.sh"
	fi
	echo '```'
	echo
	echo "## Method"
	echo
	echo "One key, **${RATE} req/s for ${DURATION}s** against \`burst-tier-token-bucket\` (refill"
	echo "${REFILL_RATE}/s, so every request is admitted while the stack is healthy), **$SCALE"
	echo "replicas** behind Traefik. From **t+${FIRST_RESTART_AT}s**, each replica in turn is"
	echo "restarted with \`docker restart -t 20\` (SIGTERM; the replica fails \`/readyz\`, keeps"
	echo "serving for its 6s drain delay, then drains in flight and exits; Docker starts"
	echo "it again) and the next restart waits until the previous replica is healthy and"
	echo "has had one Traefik check interval to rejoin the pool. Responses are bucketed"
	echo "per second from vegeta's own timestamps; restart stamps are from the host."
	echo
	echo "**Dropped** = any 5xx (a replica's 503, or Traefik's 502/504 when it had"
	echo "nowhere to route) plus any request with no HTTP response at all."
	echo
	echo "## Result"
	echo
	echo "**$DROPPED of $SENT requests dropped** across $SCALE rolling restarts"
	echo "(5xx: $TOTAL_5XX, no response: $TOTAL_OTHER; 429: $TOTAL_DENIED, which must be 0 for the"
	echo "run to be valid)."
	echo
	echo "vegeta status-code histogram: \`$STATUS_CODES\`"
	echo
	echo "### Restart timeline (generator seconds; t+0 is the first request)"
	echo
	echo "| replica | SIGTERM sent | back up | healthy and re-pooled |"
	echo "|---|---|---|---|"
	for stamp in "${RESTART_STAMPS[@]}"; do
		IFS='|' read -r name began restarted pooled <<<"$stamp"
		echo "| \`$name\` | t+$((began - FIRST_EPOCH))s | t+$((restarted - FIRST_EPOCH))s | t+$((pooled - FIRST_EPOCH))s |"
	done
	echo
	echo "Host stamped the generator's launch at $(printf 't%+ds' "$((T0_HOST - FIRST_EPOCH))"), so host and"
	echo "container clocks agree to within a second."
	echo
	echo "### Per-second timeline, collapsed into runs of identical shape"
	echo
	echo "| seconds | 200 allowed | 429 denied | 5xx | other (no HTTP response) |"
	echo "|---|---|---|---|---|"
	while IFS='|' read -r range ra rd re ro; do
		echo "| $range | $ra | $rd | $re | $ro |"
	done <<<"$RUNS"
	echo "| **total** | **$TOTAL_OK** | **$TOTAL_DENIED** | **$TOTAL_5XX** | **$TOTAL_OTHER** |"
	echo
	echo "Raw vegeta output, decoded JSON lines, and per-second buckets are kept under"
	echo "\`bench/results/raw/$STAMP-rolling-restart/\` (gitignored — regenerate by rerunning"
	echo "this script rather than diffing binary blobs)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"
log "dropped $DROPPED of $SENT requests"

if [[ "$KEEP_STACK" != "1" ]]; then
	log "bringing stack down"
	docker compose down --remove-orphans >/dev/null
fi
