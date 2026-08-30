#!/usr/bin/env bash
#
# bench/run-microbench.sh — Go microbenchmarks, repeated and summarised with
# benchstat.
#
# The innermost two layers of the three-layer benchmark story (ADR-0001):
#
#   1. internal/limiter — pure algorithm cost, no store, no HTTP, no container.
#   2. internal/rules   — a full Engine.Check against real Redis via
#                         testcontainers (build tag `bench`, needs Docker).
#
# Both layers are measured with `-count=N` (default 10) and reduced by
# benchstat rather than reported from a single run. benchstat's own
# documentation is explicit that one run is not enough: it asks for "at least
# 10 times" so it can report a median with a confidence interval instead of a
# point estimate, which on a thermally-throttled laptop sharing cores with
# Docker is the difference between a measurement and an anecdote.
#
# Usage: bench/run-microbench.sh [--count 10] [--benchtime 1s] [--skip-engine]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$ROOT/bench"
cd "$ROOT"

COUNT="10"
BENCHTIME="1s"
SKIP_ENGINE="0"
INVOCATION=("$@")

while [[ $# -gt 0 ]]; do
	case "$1" in
	--count)
		COUNT="$2"
		shift 2
		;;
	--benchtime)
		BENCHTIME="$2"
		shift 2
		;;
	--skip-engine)
		SKIP_ENGINE="1"
		shift
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 1
		;;
	esac
done

command -v go >/dev/null || {
	echo "go not found on PATH" >&2
	exit 1
}

# benchstat is the reduction step, not a nicety — without it this script would
# emit ten unsummarised runs per benchmark and leave the reader to eyeball a
# median, which is the anecdote problem it exists to fix.
BENCHSTAT="$(command -v benchstat || true)"
if [[ -z "$BENCHSTAT" && -x "$(go env GOPATH)/bin/benchstat" ]]; then
	BENCHSTAT="$(go env GOPATH)/bin/benchstat"
fi
if [[ -z "$BENCHSTAT" ]]; then
	echo "benchstat not found (go install golang.org/x/perf/cmd/benchstat@latest)" >&2
	exit 1
fi

if [[ "$SKIP_ENGINE" != "1" ]]; then
	command -v docker >/dev/null || {
		echo "docker not found on PATH — needed for the engine+Redis layer (testcontainers); pass --skip-engine to measure the pure-algorithm layer only" >&2
		exit 1
	}
fi

STAMP="$(date +%Y-%m-%d-%H%M%S)"
HOST_TAG="$(hostname -s 2>/dev/null || hostname)"
RESULTS_DIR="$BENCH_DIR/results"
RAW_DIR="$RESULTS_DIR/raw/$STAMP-microbench"
mkdir -p "$RAW_DIR"
RESULTS_FILE="$RESULTS_DIR/$STAMP-$HOST_TAG-microbench.md"

log() { echo "[bench-microbench] $*" >&2; }

# run_bench <label> <raw-basename> <go-test-args...> — runs one package's
# benchmarks -count times, tees the raw `go test -bench` output (which is what
# benchstat parses) into RAW_DIR, and leaves the path in LAST_RAW.
run_bench() {
	local label="$1" raw_name="$2"
	shift 2
	LAST_RAW="$RAW_DIR/$raw_name.txt"
	log "running $label (-count=$COUNT -benchtime=$BENCHTIME)"
	# -run='^$' matches no test, so the binary runs benchmarks only and unit
	# tests never land inside the measured wall time.
	go test -run='^$' -bench=. -benchmem -count="$COUNT" -benchtime="$BENCHTIME" "$@" |
		tee "$LAST_RAW"
}

run_bench "pure-algorithm layer (internal/limiter)" limiter ./internal/limiter/...
LIMITER_RAW="$LAST_RAW"

# Read out of the test source so the prose can't drift from the constant. Done
# here rather than inside the results-file block: under `set -euo pipefail` a
# failed grep there (constant renamed) would kill the script mid-write and
# leave a silently truncated results file.
BENCH_NUM_KEYS="$(grep -o 'benchNumKeys = [0-9]*' internal/limiter/bench_test.go | head -n1 | tr -cd '0-9' || true)"
: "${BENCH_NUM_KEYS:=many}"

ENGINE_RAW=""
if [[ "$SKIP_ENGINE" != "1" ]]; then
	run_bench "engine+Redis layer (internal/rules, real Redis via testcontainers)" engine \
		-tags bench ./internal/rules/...
	ENGINE_RAW="$LAST_RAW"
fi

# --- write results file -----------------------------------------------------
{
	echo "# Go microbenchmarks — $STAMP"
	echo
	echo "## Hardware"
	echo
	echo "- Host: \`$(uname -a)\`"
	if command -v sysctl >/dev/null && sysctl -n machdep.cpu.brand_string >/dev/null 2>&1; then
		echo "- CPU: \`$(sysctl -n machdep.cpu.brand_string)\` ($(sysctl -n hw.ncpu) logical cores)"
	fi
	echo "- Go: \`$(go version)\`"
	if [[ "$SKIP_ENGINE" != "1" ]]; then
		echo "- Docker: \`$(docker version --format '{{.Server.Version}}')\`"
	fi
	echo
	echo "## Command"
	echo
	echo '```'
	if ((${#INVOCATION[@]} > 0)); then
		echo "bench/run-microbench.sh ${INVOCATION[*]}"
	else
		echo "bench/run-microbench.sh"
	fi
	echo '```'
	echo
	echo "## Method"
	echo
	echo "Every benchmark below was run **$COUNT times** (\`-count=$COUNT\`) at"
	echo "\`-benchtime=$BENCHTIME\` and reduced with"
	echo "[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat), whose"
	echo "documentation asks for at least 10 runs before it will report a median and a"
	echo "confidence interval. The \`±\` column is that interval, not a standard"
	echo "deviation: it is what says whether a difference between two rows is real or is"
	echo "the laptop's scheduler and thermal state. Single-run benchmark output is not"
	echo "published here for that reason."
	echo
	echo "No baseline/experiment comparison is run: these are absolute cost tables for"
	echo "one commit, so benchstat prints medians with intervals rather than a delta and"
	echo "a p-value. Comparing two commits is \`benchstat old.txt new.txt\` over the raw"
	echo "files kept alongside this one."
	echo
	echo "## Layer 1 — pure algorithm cost (\`internal/limiter\`)"
	echo
	echo "No store, no HTTP, no container. Serial rows are uncontended per-operation"
	echo "cost; parallel rows spread concurrent callers across $BENCH_NUM_KEYS keys through the"
	echo "algorithm's manager, which is what exposes the per-key routing lock."
	echo
	# benchstat labels each column with the file path it was given unless an
	# explicit `label=path` is used; the bare path is an absolute machine-local
	# one, which would make the committed table both ugly and non-portable.
	echo '```'
	"$BENCHSTAT" "pure-algorithm=$LIMITER_RAW"
	echo '```'
	echo
	if [[ -n "$ENGINE_RAW" ]]; then
		echo "## Layer 2 — full rule evaluation against real Redis (\`internal/rules\`)"
		echo
		echo "Rule match + Lua execution + round trip, against a real Redis container"
		echo "(ADR-0001: a fake would measure Go function-call overhead and report it as"
		echo "Lua execution cost). The uncached/cached pair is the same"
		echo "\`burst-tier-token-bucket\` / \`cached-tier-token-bucket\` A/B used elsewhere:"
		echo "identical capacity and refill rate, differing only in \`local_cache\`."
		echo
		echo '```'
		"$BENCHSTAT" "engine+redis=$ENGINE_RAW"
		echo '```'
		echo
	else
		echo "## Layer 2 — skipped"
		echo
		echo "Run with \`--skip-engine\`; the engine+Redis layer was not measured."
		echo
	fi
	echo "Raw \`go test -bench\` output (the input benchstat parsed) is kept under"
	echo "\`bench/results/raw/$STAMP-microbench/\` (gitignored — regenerate by rerunning"
	echo "this script)."
} >"$RESULTS_FILE"

log "wrote $RESULTS_FILE"
