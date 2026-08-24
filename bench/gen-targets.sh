#!/usr/bin/env bash
#
# bench/gen-targets.sh — writes a vegeta JSON-lines targets file for POST
# /v1/check, cycling through a given number of distinct keys (CONTEXT.md,
# "cardinality"). vegeta loops over a targets file for the duration of an
# attack, so a 1-key file repeats the same key on every request (single-bucket
# contention) and a 10k-key file spreads load across 10k buckets (map growth
# and Redis keyspace behaviour) — the two points this project reports and
# never blends.
#
# Usage: gen-targets.sh <url> <x-plan-value> <key-count> <output-file>
set -euo pipefail

if [[ $# -ne 4 ]]; then
	echo "usage: gen-targets.sh <url> <x-plan-value> <key-count> <output-file>" >&2
	exit 1
fi

URL="$1"
PLAN="$2"
KEY_COUNT="$3"
OUT="$4"

# One jq process building all lines, rather than one process per key — the
# 10k-key files would otherwise take the better part of a minute each to
# generate.
jq -nc \
	--arg url "$URL" \
	--arg plan "$PLAN" \
	--argjson count "$KEY_COUNT" \
	'range($count) as $i
	 | ({key: ("bench-key-" + ($i | tostring))} | tojson | @base64) as $body
	 | {method: "POST", url: $url, header: {"Content-Type": ["application/json"], "X-Plan": [$plan]}, body: $body}' \
	>"$OUT"
