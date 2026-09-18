#!/usr/bin/env bash
#
# bench/run-failure-mode.sh — Redis-outage / failure-mode harness.
#
# Measures how Limigo behaves while its backing store is unreachable: how
# quickly decisions flip from allowed to denied, what the API returns and under
# which metric label for the duration, and how long recovery takes once the
# store is back. Writes the results file the README's §7 reports from.
#
# Two arms, the same controlled pair every other harness uses
# (config.example.yaml: burst-tier-token-bucket and cached-tier-token-bucket,
# both capacity 1000 / rate 200/s, identical except local_cache), because they
# are expected to fail *differently* and the difference is the finding:
#
#   local_cache: false — every request round-trips to Redis, so the first
#     request after the outage starts has nowhere to go. Expect an immediate
#     flip to 503 with allowed=false, recorded under the metric's
#     result="error" label rather than result="denied".
#
#   local_cache: true  — the request path never touches Redis, so the node
#     keeps deciding from its local baseline. That baseline is a snapshot and
#     does not simulate refill (see limiter.BatchingTokenBucket), so the arm
#     should serve normally until the snapshot is spent and then deny with 429
#     — a rate-limit answer, not an error — for the rest of the outage.
#     Expected survival = capacity / probe rate seconds, independent of
#     hardware.
#
# Fixed at one replica. The comparison is per-node degradation, and the
# Prometheus counters below are read from a single replica's /metrics — with
# --scale limigo=N the service DNS name would round-robin between replicas and
# the snapshots would describe whichever one answered.
#
# The probe rate must stay below the shared 200/s refill so that, with Redis
# healthy, both arms sit permanently on the allow path: any non-200 in the
# results is then attributable to the outage and nothing else.
#
# Both arms are attacked concurrently for the whole run and the transitions
# are located afterwards in the per-request timeline, rather than run as three
# separate before/during/after attacks. The flip and recovery latencies are
# the deliverable here, and an attack that starts after the outage does can't
# measure when the outage started to bite.
#
# Usage: bench/run-failure-mode.sh [--rate 100] [--baseline 10] [--outage 25]
#                                  [--recovery 15] [--keep-stack]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

RATE="100"
BASELINE_S="10"
OUTAGE_S="25"
RECOVERY_S="15"
KEEP_STACK="0"
INVOCATION=("$@")

while [[ $# -gt 0 ]]; do
	case "$1" in
	--rate)
		RATE="$2"
		shift 2
		;;
	--baseline)
		BASELINE_S="$2"
		shift 2
		;;
	--outage)
		OUTAGE_S="$2"
		shift 2
		;;
	--recovery)
		RECOVERY_S="$2"
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

# require_positive_int <flag> <value> — bash arithmetic reads a non-numeric
# argument as 0 and says nothing, and 0 is a live setting for all four of
# these: `-rate=0` tells vegeta to send as fast as it can, which would swamp
# both arms into denial before the outage began and then publish that as a
# failure-mode measurement.
require_positive_int() {
	if ! [[ "$2" =~ ^[1-9][0-9]*$ ]]; then
		echo "$1 must be a positive whole number, got \"$2\"" >&2
		exit 1
	fi
}

require_positive_int --rate "$RATE"
require_positive_int --baseline "$BASELINE_S"
require_positive_int --outage "$OUTAGE_S"
require_positive_int --recovery "$RECOVERY_S"

# The controlled A/B pair (config.example.yaml) — hardcoded because the
# expected-survival figure below depends on these exact numbers matching that
# file. If that file's burst-tier / cached-tier token-bucket pair ever
# changes, this must change with it.
CAPACITY=1000
REFILL_RATE=200

if ((RATE >= REFILL_RATE)); then
	echo "--rate ($RATE) must stay below the rules' shared refill rate ($REFILL_RATE/s), or both arms would be denying requests before the outage starts and no non-200 could be attributed to it" >&2
	exit 1
fi

# With Redis unreachable a cached bucket can only spend the snapshot it
# already holds, and its local view never refills (limiter.BatchingTokenBucket
# defers refill entirely to the store). At a probe rate held below the refill
# rate the snapshot sits at capacity when the outage starts, so this is how
# long the cached arm should keep admitting — the same figure on any hardware.
EXPECTED_CACHED_SURVIVAL_S="$(awk -v c="$CAPACITY" -v r="$RATE" 'BEGIN { printf "%.1f", c / r }')"

TOTAL_S=$((BASELINE_S + OUTAGE_S + RECOVERY_S))

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP-failure-mode"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG-failure-mode.md"

PROBE_CONTAINER="limigo-bench-probe-$STAMP"

log() { echo "[bench-failure-mode] $*" >&2; }

cleanup() {
	docker rm -f "$PROBE_CONTAINER" >/dev/null 2>&1 || true
	# A run that aborts mid-outage would otherwise leave Redis stopped and the
	# stack in the one state no other harness expects to inherit.
	docker compose start redis >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "building the vegeta generator image"
docker build -q -f "$BENCH_DIR/Dockerfile.vegeta" -t limigo-bench-vegeta "$BENCH_DIR" >/dev/null

# --- stack -------------------------------------------------------------------
# compute_cpusets is identical to bench/run-overshoot.sh's: split the CPUs
# Docker reports in half so the generator never competes with limigo for cores.
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

compute_cpusets

log "bringing stack down for a clean start"
docker compose down --remove-orphans >/dev/null
log "starting stack: docker compose up -d --build --wait (1 limigo replica)"
docker compose up -d --build --wait >/dev/null

PROJECT="$(docker compose config --format json | jq -r '.name')"
NETWORK="${PROJECT}_default"
docker update --cpuset-cpus="$LIMIGO_CPUSET" "$(docker compose ps -q limigo)" >/dev/null
log "pinned limigo to CPUs $LIMIGO_CPUSET; generator uses $GEN_CPUSET"

# See bench/run-throughput.sh bring_up: Traefik's routing table lags the
# replica set by a few seconds after a fresh start.
log "settling: waiting for Traefik's routing table to catch up"
sleep 5

# A long-lived helper on the compose network, so limigo's /metrics can be read
# during the outage — the stack's other shell-bearing container is redis,
# which is by definition unavailable then. Reuses the generator image (its
# base carries busybox) rather than pulling another one, keeping the harness's
# external image dependencies where Dockerfile.vegeta already put them.
log "starting the probe container"
docker run -d --name "$PROBE_CONTAINER" --network "$NETWORK" --cpuset-cpus="$GEN_CPUSET" \
	--entrypoint sleep limigo-bench-vegeta "$((TOTAL_S + 600))" >/dev/null

# --- clock alignment ---------------------------------------------------------
# vegeta timestamps inside the Docker VM; the outage is triggered from the
# host. Rather than subtract one clock from the other and hope they agree,
# every figure below is anchored to a marker *inside* the vegeta timeline —
# see locate_outage. This check exists only to confirm the two clocks are in
# the same phase, so those markers can be trusted to sit in the run segment
# they claim to. Busybox has no sub-second `date`, so a whole second is the
# finest this can resolve, and a whole second is all it needs to resolve.
CLOCK_SKEW_S=$(($(docker exec "$PROBE_CONTAINER" date +%s) - $(date +%s)))
if ((CLOCK_SKEW_S > 2 || CLOCK_SKEW_S < -2)); then
	echo "host and container clocks differ by ${CLOCK_SKEW_S}s — too far apart to trust which run segment a timestamp falls in" >&2
	exit 1
fi
log "host/container clock agreement: ${CLOCK_SKEW_S}s"

# --- probes ------------------------------------------------------------------
PROXY_URL="http://traefik/v1/check"
UNCACHED_PLAN="burst"
CACHED_PLAN="cached"

# Single key per arm: outage behaviour is a property of one bucket's state,
# and spreading over 10k keys would only average that state away.
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "$UNCACHED_PLAN" 1 "$RAW_DIR/uncached.jsonl"
bash "$BENCH_DIR/gen-targets.sh" "$PROXY_URL" "$CACHED_PLAN" 1 "$RAW_DIR/cached.jsonl"

# attack <name> — open-model, fixed arrival rate, no worker cap, for the whole
# run. No -max-workers: the point of this harness is the moments when the
# service stops answering normally, and a capped pool is exactly what stops
# those moments from being sent and timed (see README, coordinated omission).
attack() {
	local name="$1"
	docker run --rm \
		--network "$NETWORK" \
		--cpuset-cpus="$GEN_CPUSET" \
		-v "$RAW_DIR:/raw" \
		limigo-bench-vegeta \
		attack -targets="/raw/$name.jsonl" -format=json \
		-rate="$RATE/1s" -duration="${TOTAL_S}s" \
		-output="/raw/$name.bin"
}

# metrics_snapshot <file> — limigo_requests_total as "result|rule count"
# lines. Read from limigo's own /metrics rather than through Prometheus: a 5s
# scrape interval cannot resolve a window this short, and the counters have to
# be readable while Redis is down.
metrics_snapshot() {
	local out="$1"
	docker exec "$PROBE_CONTAINER" wget -qO- http://limigo:9091/metrics |
		awk -F'[{}]' '/^limigo_requests_total\{/ {
			split($2, labels, ",")
			result = ""; rule = ""
			for (i in labels) {
				split(labels[i], kv, "=")
				gsub(/"/, "", kv[2])
				if (kv[1] == "result") result = kv[2]
				if (kv[1] == "rule") rule = kv[2]
			}
			printf "%s|%s %d\n", result, rule, $3 + 0
		}' | sort >"$out"
}

# redis_snapshot <file> — the authoritative bucket state for both arms, plus
# the keyspace size, so "was counter state lost across the gap" is answered
# from Redis rather than inferred from response codes.
redis_snapshot() {
	local out="$1"
	{
		echo "dbsize $(docker compose exec -T redis redis-cli DBSIZE)"
		local arm key
		for arm in "burst-tier-token-bucket" "cached-tier-token-bucket"; do
			key="limigo:$arm:bench-key-0"
			echo "$arm tokens=$(docker compose exec -T redis redis-cli HGET "$key" tokens) last_refill_ms=$(docker compose exec -T redis redis-cli HGET "$key" last_refill_ms)"
		done
	} >"$out"
}

# redis_reload_snapshot <file> — how much of the dataset the restarted Redis
# loaded from disk. Comparing bucket state before and after can't answer this
# on its own: a token bucket that lost its key is recreated at capacity, and
# one that kept its key refills to capacity across any gap longer than
# capacity/rate, so both roads lead to a full bucket. `INFO persistence`
# reports the keys actually read back at startup, which distinguishes them.
redis_reload_snapshot() {
	docker compose exec -T redis redis-cli INFO persistence |
		grep -E 'rdb_last_load_keys_loaded|rdb_last_load_keys_expired|rdb_changes_since_last_save|loading:' |
		tr -d '\r' >"$1"
}

# --- run ---------------------------------------------------------------------
log "starting both arms: $RATE req/s for ${TOTAL_S}s (${BASELINE_S}s baseline, ${OUTAGE_S}s outage, ${RECOVERY_S}s recovery)"
attack uncached &
UNCACHED_PID=$!
attack cached &
CACHED_PID=$!

sleep "$BASELINE_S"

redis_snapshot "$RAW_DIR/redis-before.txt"

# `stop -t 0` rather than `kill`: docker-compose.yml gives redis
# `restart: unless-stopped`, so a plain kill would be undone by the restart
# policy within seconds and there would be no outage to measure. `stop` marks
# the container as intentionally stopped; `-t 0` skips the graceful SIGTERM
# window, so this is an abrupt loss of the store and not a clean shutdown that
# gets to flush its dataset on the way out.
log "stopping redis"
KILL_BEGIN_NS="$(date +%s%N)"
docker compose stop -t 0 redis >/dev/null
KILL_END_NS="$(date +%s%N)"

# The metric snapshots bracket the outage from *inside* it — taken after the
# stop command returns and after the start command returns, rather than either
# side of the sleep. Both commands take seconds on this stack, and a snapshot
# taken before the stop would fold that healthy head and tail into the outage
# delta, which is exactly how a table ends up reporting hundreds of allowed
# requests during an outage.
metrics_snapshot "$RAW_DIR/metrics-outage-start.txt"

sleep "$OUTAGE_S"

log "starting redis"
RESTORE_BEGIN_NS="$(date +%s%N)"
docker compose start redis >/dev/null
RESTORE_END_NS="$(date +%s%N)"

metrics_snapshot "$RAW_DIR/metrics-outage-end.txt"
redis_reload_snapshot "$RAW_DIR/redis-reload.txt"

wait "$UNCACHED_PID" "$CACHED_PID"

metrics_snapshot "$RAW_DIR/metrics-after.txt"
redis_snapshot "$RAW_DIR/redis-after.txt"

# --- analysis ----------------------------------------------------------------
# vegeta's CSV encoding puts the send timestamp (unix ns) first and the status
# code second. Only those two fields are taken: later fields can contain
# commas and are quoted, but quoting never shifts the position of the fields
# to their left.
#
# vegeta writes results in completion order, not send order, so a slow
# response can land in the file ahead of faster ones sent after it. Every
# boundary below is "the first result on one side of an instant", which reads
# the wrong row entirely off an unsorted file — the first run of this harness
# reported 19 failures before an outage that had not started yet. Sorting by
# the send timestamp is what makes the file a timeline rather than a log.
timeline() {
	local name="$1"
	docker run --rm -v "$RAW_DIR:/raw" limigo-bench-vegeta encode --to csv "/raw/$name.bin" |
		cut -d, -f1,2 | sort -t, -k1,1n >"$RAW_DIR/$name.timeline.csv"
}

timeline uncached
timeline cached

# The outage boundaries are read out of the uncached arm's own timeline rather
# than taken from the host clock. Every uncached decision requires a live
# round-trip, so that arm's first failure is the first probe issued after the
# store became unreachable, and its first success afterwards is the first
# probe issued after the store came back — both accurate to the probe
# interval, and both in the same clock as every timestamp they are subtracted
# from. Projecting the host's stop/start instants into the generator's clock
# instead would put a skew of unknown size (the check above resolves it only
# to the second) underneath a measurement whose interesting figures are
# milliseconds.
locate_outage() {
	local marker
	marker="$(awk -F, '$2 != 200 { print $1; exit }' "$RAW_DIR/uncached.timeline.csv")"
	if [[ -z "$marker" ]]; then
		echo "FATAL: the uncached arm never failed — no outage was observed, so there is nothing to measure. Check that 'docker compose stop -t 0 redis' actually stopped the container." >&2
		exit 1
	fi
	T_DOWN_NS="$marker"
	marker="$(awk -F, -v down="$T_DOWN_NS" '$1 + 0 > down && $2 == 200 { print $1; exit }' "$RAW_DIR/uncached.timeline.csv")"
	if [[ -z "$marker" ]]; then
		echo "FATAL: the uncached arm never recovered — Redis did not come back within the run window. Try a longer --recovery." >&2
		exit 1
	fi
	T_UP_NS="$marker"
}

locate_outage
OUTAGE_OBSERVED_S="$(awk -v a="$T_DOWN_NS" -v b="$T_UP_NS" 'BEGIN { printf "%.2f", (b - a) / 1000000000 }')"
log "outage observed from the uncached arm: ${OUTAGE_OBSERVED_S}s"

# analyse <name> — reduces one arm's timeline to a single pipe-separated row:
#
#   baseline_2xx|baseline_other|flip_ms|first_outage_code|outage_200|outage_429|
#   outage_503|outage_other|recovery_ms|residual_failures|post_200|post_other
#
# flip_ms is the delay from the outage marker to this arm's first non-200, and
# recovery_ms the delay from the recovery marker to its first 200 after it.
# residual_failures counts non-200s after that first success — a recovery that
# is not monotonic would be a finding, and averaging it into recovery_ms would
# hide it.
analyse() {
	local name="$1"
	awk -F, -v kill_ns="$T_DOWN_NS" -v restore_ns="$T_UP_NS" '
	{
		ts = $1 + 0; code = $2 + 0
		if (ts < kill_ns) {
			if (code == 200) base_ok++; else base_other++
			next
		}
		if (ts < restore_ns) {
			if (flip_ns == 0 && code != 200) { flip_ns = ts; flip_code = code }
			if (code == 200) out200++
			else if (code == 429) out429++
			else if (code == 503) out503++
			else outother++
			next
		}
		if (recovered_ns == 0) {
			if (code == 200) recovered_ns = ts
		} else if (code != 200) residual++
		if (code == 200) post_ok++; else post_other++
	}
	END {
		flip_ms = (flip_ns == 0 ? -1 : (flip_ns - kill_ns) / 1000000)
		rec_ms = (recovered_ns == 0 ? -1 : (recovered_ns - restore_ns) / 1000000)
		printf "%d|%d|%.1f|%s|%d|%d|%d|%d|%.1f|%d|%d|%d\n",
			base_ok, base_other, flip_ms, (flip_code ? flip_code : "none"),
			out200, out429, out503, outother, rec_ms, residual, post_ok, post_other
	}' "$RAW_DIR/$name.timeline.csv"
}

UNCACHED_ROW="$(analyse uncached)"
CACHED_ROW="$(analyse cached)"

row_field() { cut -d'|' -f"$2" <<<"$1"; }

# duration <ms> — renders one of analyse's millisecond fields for the results
# tables. A negative value is analyse's "this never happened" sentinel.
duration() {
	awk -v ms="$1" 'BEGIN { if (ms < 0) print "never"; else if (ms < 1000) printf "%.0fms", ms; else printf "%.2fs", ms / 1000 }'
}

# metric_delta <before> <after> — the counter increments between two
# snapshots, as "result|rule count" lines. Series absent from the earlier
# snapshot start at zero; a series that only appears in the earlier one
# contributed nothing and is dropped.
metric_delta() {
	awk 'NR == FNR { before[$1] = $2; next }
	     { d = $2 - (($1 in before) ? before[$1] : 0); if (d > 0) printf "%s %d\n", $1, d }' "$1" "$2"
}

metric_delta "$RAW_DIR/metrics-outage-start.txt" "$RAW_DIR/metrics-outage-end.txt" >"$RAW_DIR/delta-outage.txt"
metric_delta "$RAW_DIR/metrics-outage-end.txt" "$RAW_DIR/metrics-after.txt" >"$RAW_DIR/delta-recovery.txt"

# metric_rows <delta-file> — the delta as markdown table rows.
metric_rows() {
	awk -F'[| ]' '{ printf "| `%s` | `%s` | %d |\n", $2, $1, $3 }' "$1" | sort
}

# Each arm has a claim the README makes on its behalf, and a run that
# contradicts one is a design finding rather than a number to fold quietly
# into a results table. The uncached arm's flip time is not among them — it
# defines the marker (see locate_outage) and so cannot disagree with it.
FINDINGS="$(awk -v flip_ms="$(row_field "$CACHED_ROW" 3)" \
	-v rec_ms="$(row_field "$CACHED_ROW" 9)" \
	-v expected_s="$EXPECTED_CACHED_SURVIVAL_S" \
	-v uncached_code="$(row_field "$UNCACHED_ROW" 4)" \
	-v uncached_429="$(row_field "$UNCACHED_ROW" 6)" \
	-v cached_503="$(row_field "$CACHED_ROW" 7)" 'BEGIN {
	if (flip_ms < 0) {
		printf "cached arm never flipped: it admitted every request for the whole outage, so the local baseline is not being spent as expected\n"
	} else {
		observed = flip_ms / 1000
		ratio = (expected_s > 0 ? observed / expected_s : 0)
		if (ratio < 0.5 || ratio > 2) {
			printf "cached arm survived %.1fs against an expected %.1fs (%.2fx) — the local baseline is not draining the way limiter.BatchingTokenBucket documents\n", observed, expected_s, ratio
		}
	}
	if (rec_ms < 0) {
		printf "cached arm never returned a 200 after the store came back — a node that stays wedged past recovery is worse than one that fails during the outage\n"
	}
	if (uncached_code != 503) {
		printf "uncached arm failed with %s, not the 503 internal/api/check.go returns for a store error — a store outage is being reported to callers as something else\n", uncached_code
	}
	if (uncached_429 > 0) {
		printf "uncached arm returned %d 429s during the outage — a store failure was reported as a rate-limit denial, which is exactly the confusion the error/denied metric split exists to prevent\n", uncached_429
	}
	if (cached_503 > 0) {
		printf "cached arm returned %d 503s during the outage — its request path is supposed to be able to answer without the store at all\n", cached_503
	}
}')"

# --- write results file ------------------------------------------------------
{
	echo "# Failure mode — Redis outage — $STAMP"
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
		echo "bench/run-failure-mode.sh ${INVOCATION[*]}"
	else
		echo "bench/run-failure-mode.sh"
	fi
	echo '```'
	echo
	echo "## Method"
	echo
	echo "One limigo replica against \`config.example.yaml\`'s controlled pair:"
	echo "\`burst-tier-token-bucket\` (no local cache) and \`cached-tier-token-bucket\`"
	echo "(identical — capacity $CAPACITY, rate ${REFILL_RATE}/s — except \`local_cache: true\`)."
	echo "Both arms are attacked concurrently at **$RATE req/s** against a single key for"
	echo "**${TOTAL_S}s**, open-model with no worker cap. The probe rate is held below the"
	echo "rules' shared ${REFILL_RATE}/s refill, so with Redis healthy every request is on the"
	echo "allow path and any non-200 belongs to the outage."
	echo
	echo "Redis is stopped ${BASELINE_S}s in (\`docker compose stop -t 0 redis\` — abrupt, and not"
	echo "the \`kill\` the container's \`restart: unless-stopped\` policy would immediately"
	echo "undo) and started again ${OUTAGE_S}s later, leaving ${RECOVERY_S}s of recovery in the window."
	echo
	echo "**Where the outage window comes from.** vegeta timestamps inside the Docker VM"
	echo "and the outage is triggered from the host, so the two boundary instants are not"
	echo "taken from the host clock at all. They are read out of the **uncached arm's own"
	echo "timeline**: every uncached decision needs a live round-trip, so that arm's first"
	echo "failure is the first probe issued after the store went away and its first"
	echo "success afterwards is the first probe issued after it came back. Both markers"
	echo "therefore sit in the same clock as every timestamp subtracted from them, and the"
	echo "$(awk -v r="$RATE" 'BEGIN { printf "%.0f", 1000 / r }')ms gap between probes at $RATE req/s is the floor on every figure below."
	echo "The consequence is that the uncached arm cannot measure its own flip time — it"
	echo "*defines* the zero — so its row reads \`marker\` rather than a suspiciously round"
	echo "0ms. What that row does measure is which failure the arm produced and for how"
	echo "long, which is the part in question."
	echo
	echo "Host and container clocks were checked to agree within **${CLOCK_SKEW_S}s** (busybox has no"
	echo "sub-second \`date\`, so a second is as fine as this resolves), which is enough to"
	echo "confirm the markers fall in the run segment they claim to. The observed outage"
	echo "was **${OUTAGE_OBSERVED_S}s** long against the ${OUTAGE_S}s requested; the stop command itself took"
	echo "$(awk -v n="$((KILL_END_NS - KILL_BEGIN_NS))" 'BEGIN { printf "%.0f", n / 1000000 }')ms and the start command $(awk -v n="$((RESTORE_END_NS - RESTORE_BEGIN_NS))" 'BEGIN { printf "%.0f", n / 1000000 }')ms, which is where that difference comes from."
	echo
	echo "**No control row here, deliberately.** Every other results file in this"
	echo "directory opens with a \`/healthz\` run, because it publishes throughput or"
	echo "latency and those are meaningless without the ceiling the environment imposes"
	echo "(CONTEXT.md: control run, ceiling). Nothing below is a rate or a percentile —"
	echo "the figures are response codes, counts, and two transition times — so there is"
	echo "no ceiling to report them against. What plays the control's role instead is the"
	echo "uncached arm, which differs from the cached arm in exactly one config field."
	echo
	echo "**Expected cached survival:** a cached bucket decides against a snapshot of the"
	echo "authoritative token count and does not simulate refill locally"
	echo "(\`limiter.BatchingTokenBucket\`), so with the snapshot sitting at capacity when"
	echo "the store disappears it can admit \`capacity / rate\` = $CAPACITY / $RATE ="
	echo "**${EXPECTED_CACHED_SURVIVAL_S}s** of requests before it must start denying. Hardware-independent,"
	echo "like the overshoot harness's expected ceiling."
	echo
	echo "## Results — response behaviour"
	echo
	echo "| arm | baseline 200s | time to first non-200 | first failure code | outage: 200 | outage: 429 | outage: 503 | outage: other |"
	echo "|---|---|---|---|---|---|---|---|"
	echo "| local_cache: false | $(row_field "$UNCACHED_ROW" 1) | marker | $(row_field "$UNCACHED_ROW" 4) | $(row_field "$UNCACHED_ROW" 5) | $(row_field "$UNCACHED_ROW" 6) | $(row_field "$UNCACHED_ROW" 7) | $(row_field "$UNCACHED_ROW" 8) |"
	echo "| local_cache: true | $(row_field "$CACHED_ROW" 1) | $(duration "$(row_field "$CACHED_ROW" 3)") | $(row_field "$CACHED_ROW" 4) | $(row_field "$CACHED_ROW" 5) | $(row_field "$CACHED_ROW" 6) | $(row_field "$CACHED_ROW" 7) | $(row_field "$CACHED_ROW" 8) |"
	echo
	for pair in "local_cache: false|$UNCACHED_ROW" "local_cache: true|$CACHED_ROW"; do
		row="${pair#*|}"
		base_other="$(row_field "$row" 2)"
		if ((base_other > 0)); then
			echo "> **${pair%%|*}** saw $base_other non-200 responses *before* the outage began; the baseline was not clean and the outage columns above are contaminated."
			echo
		fi
	done
	echo "## Results — recovery"
	echo
	echo "| arm | back to 200 | post-restart 200s | post-restart non-200s | of those, after the first 200 |"
	echo "|---|---|---|---|---|"
	echo "| local_cache: false | marker | $(row_field "$UNCACHED_ROW" 11) | $(row_field "$UNCACHED_ROW" 12) | $(row_field "$UNCACHED_ROW" 10) |"
	echo "| local_cache: true | $(duration "$(row_field "$CACHED_ROW" 9)") | $(row_field "$CACHED_ROW" 11) | $(row_field "$CACHED_ROW" 12) | $(row_field "$CACHED_ROW" 10) |"
	echo
	echo "The last two columns are not the same number twice. Every non-200 in the"
	echo "post-restart segment is counted first, then only those that landed *after* the"
	echo "arm's first success — a denial while an arm is still waiting to recover is"
	echo "expected, a failure after it has already recovered is not, and collapsing them"
	echo "into one column would hide whichever is the interesting one."
	echo
	echo "## Results — \`limigo_requests_total\` during the outage"
	echo
	echo "Counter increments between a scrape taken as soon as the stop command returned"
	echo "and one taken as soon as the start command returned, read from limigo's own"
	echo "\`/metrics\` rather than through Prometheus (a 5s scrape interval cannot resolve a"
	echo "window this short, and these counters have to be readable while Redis is down)."
	echo "The \`error\` and \`denied\` labels exist to be told apart here"
	echo "(\`internal/api/check.go\`): \`error\` means the store failed, \`denied\` means the"
	echo "limiter worked."
	echo
	echo "| rule | result | requests |"
	echo "|---|---|---|"
	metric_rows "$RAW_DIR/delta-outage.txt"
	echo
	echo "These two scrapes bracket the outage by shell command, not by the marker the"
	echo "tables above use, so the window can run a few milliseconds wide at each end and"
	echo "pick up a handful of requests from either side of it. Where the two disagree by"
	echo "single digits, the timeline is the precise one; this table is here for the"
	echo "\`result\` labels, which the timeline cannot see."
	echo
	echo "And between the start command returning and the end of the run:"
	echo
	echo "| rule | result | requests |"
	echo "|---|---|---|"
	metric_rows "$RAW_DIR/delta-recovery.txt"
	echo
	echo "## Results — counter state across the gap"
	echo
	echo "Authoritative bucket state read from Redis before the stop and after the run."
	echo "A \`tokens\` field that is empty means the key does not exist."
	echo
	echo "Before the outage:"
	echo
	echo '```'
	cat "$RAW_DIR/redis-before.txt"
	echo '```'
	echo
	echo "After recovery:"
	echo
	echo '```'
	cat "$RAW_DIR/redis-after.txt"
	echo '```'
	echo
	echo "What the restarted Redis read back off disk, sampled the moment it came up:"
	echo
	echo '```'
	cat "$RAW_DIR/redis-reload.txt"
	echo '```'
	echo
	echo "This last block is load-bearing, because the bucket state on its own cannot"
	echo "answer whether the dataset survived: a token bucket whose key was lost is"
	echo "recreated at capacity, and one whose key survived refills to capacity across any"
	echo "gap longer than \`capacity / rate\` = $(awk -v c="$CAPACITY" -v r="$REFILL_RATE" 'BEGIN { printf "%.1f", c / r }')s. Both roads lead to a full bucket, so for"
	echo "this algorithm and this outage length the two are indistinguishable from the"
	echo "outside. \`rdb_last_load_keys_loaded\` is the direct answer."
	echo
	echo "**Expect this one figure to differ between runs, and do not treat a change in it"
	echo "as a regression.** \`docker compose stop -t 0\` sends SIGTERM and follows it with"
	echo "SIGKILL immediately, while a stock \`redis:7-alpine\` has save points configured"
	echo "and so tries to write its dataset out on SIGTERM. Which one wins is a race, and"
	echo "this harness has been observed landing on both sides of it: repeated stop/start"
	echo "cycles on an idle machine lost the dataset every time, while a run under the"
	echo "full two-generator load reloaded it. The finding is the race itself — an"
	echo "unpersisted store abruptly stopped guarantees nothing either way — not whichever"
	echo "side this particular run came down on."
	echo
	echo "The cached arm admitted **$(row_field "$CACHED_ROW" 5)** requests locally during the outage. Its node"
	echo "could not flush any of them while the store was gone, and a failed flush leaves"
	echo "the pending delta intact (\`rules.compileBatchingTokenBucket\`), so those admits"
	echo "should all be charged to Redis by the first flush that succeeds after recovery"
	echo "rather than lost — the cached arm's token count after recovery, against the"
	echo "uncached arm's as a control, is where to read that off."
	echo
	if [[ -n "$FINDINGS" ]]; then
		echo "## ESCALATION"
		echo
		echo "$FINDINGS"
		echo
		echo "This is a design finding, not a number to publish quietly."
	else
		echo "## Escalation check"
		echo
		echo "Both arms degraded as designed: the uncached arm failed with the 503 that"
		echo "\`internal/api/check.go\` reserves for a store error rather than a rate-limit"
		echo "denial, the cached arm spent its local baseline within a factor of two of the"
		echo "expected ${EXPECTED_CACHED_SURVIVAL_S}s and then denied rather than errored, no cached request produced a"
		echo "store error, and both arms served 200s again after recovery. This does not"
		echo "replace human judgement of the tables above."
	fi
	echo
	echo "Raw vegeta output, per-request timelines, and the metric and Redis snapshots are"
	echo "kept under \`bench/results/raw/$STAMP-failure-mode/\` (gitignored — regenerate by"
	echo "rerunning this script rather than diffing binary blobs)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"
if [[ -n "$FINDINGS" ]]; then
	log "ESCALATION: $FINDINGS"
fi

if [[ "$KEEP_STACK" != "1" ]]; then
	# Ahead of `down`, not left to the EXIT trap: the probe container is
	# attached to the compose network, and compose cannot remove a network
	# something is still using.
	docker rm -f "$PROBE_CONTAINER" >/dev/null 2>&1 || true
	log "bringing stack down"
	docker compose down --remove-orphans >/dev/null
fi
