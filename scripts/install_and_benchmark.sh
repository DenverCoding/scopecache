#!/usr/bin/env bash
# install_and_benchmark.sh — populate and smoke-benchmark a running
# scopecache instance from any client host.
#
# Steps currently implemented:
#   1. Fill a scope with N small items via /append.
#
# Each item is a tiny JSON object (`{"i":<n>}`) so the run mostly
# measures append-throughput, not payload-encoding cost. Used to seed
# scopecache for read-side experiments without depending on wrk or
# any of the Phase 4 bench tooling.
#
# Configurable via env vars (defaults shown):
#
#   URL=http://localhost     base URL of the cache (no trailing slash)
#   COUNT=50000              how many items to insert
#   PARALLEL=16              concurrent curl processes
#   SCOPE=benchmark          scope name to write into
#
# Examples:
#
#   ./install_and_benchmark.sh                          # local, 50k items
#   URL=http://1.2.3.4 ./install_and_benchmark.sh       # against remote VPS
#   COUNT=10000 PARALLEL=32 ./install_and_benchmark.sh  # smaller, faster
#
# Runs anywhere with curl + xargs (Linux, macOS, WSL, the dev container).
# No jq dependency.

set -euo pipefail

URL="${URL:-http://localhost}"
COUNT="${COUNT:-50000}"
PARALLEL="${PARALLEL:-16}"
SCOPE="${SCOPE:-benchmark}"

echo "filling ${URL}/append — scope=${SCOPE}, count=${COUNT}, parallel=${PARALLEL}"
start_epoch=$(date +%s)

# xargs substitutes __IDX__ with each input line (1..COUNT). An
# underscored placeholder instead of the default {} so it can never
# collide with a literal JSON brace inside the payload.
seq 1 "$COUNT" | xargs -P "$PARALLEL" -I __IDX__ \
    curl -s -X POST "$URL/append" \
        -H 'Content-Type: application/json' \
        -d "{\"scope\":\"${SCOPE}\",\"payload\":{\"i\":__IDX__}}" \
        -o /dev/null

end_epoch=$(date +%s)
elapsed=$((end_epoch - start_epoch))
[ "$elapsed" -eq 0 ] && elapsed=1
rate=$((COUNT / elapsed))

echo "done in ${elapsed}s (~${rate} items/s)"
echo
echo "verify with:"
echo "  curl ${URL}/stats"
echo "  curl '${URL}/head?scope=${SCOPE}&limit=3'"
