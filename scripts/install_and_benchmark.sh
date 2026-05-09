#!/usr/bin/env bash
# install_and_benchmark.sh — populate and smoke-benchmark a running
# scopecache instance from any client host.
#
# Steps currently implemented:
#   1. Fill a scope with N small items via a single /warm bulk request.
#
# Each item is a tiny JSON object (`{"i":<n>}`) so the run mostly
# measures bulk-decode + buffer-append throughput, not payload-encoding
# cost. /warm replaces the target scope only; other scopes are left
# alone. (Use /rebuild if you want to atomically wipe the entire cache
# instead — same request shape, different endpoint.)
#
# Configurable via env vars (defaults shown):
#
#   URL=http://localhost     base URL of the cache (no trailing slash)
#   COUNT=50000              how many items to insert
#   SCOPE=benchmark          scope name to write into
#
# Examples:
#
#   ./install_and_benchmark.sh                          # local, 50k items
#   URL=http://1.2.3.4 ./install_and_benchmark.sh       # against remote VPS
#   COUNT=10000 ./install_and_benchmark.sh              # smaller dataset
#
# Runs anywhere with curl + awk (Linux, WSL, the dev container; macOS
# has BSD date so the millisecond timing falls back to second-only —
# the items still land correctly).

set -euo pipefail

URL="${URL:-http://localhost}"
COUNT="${COUNT:-50000}"
SCOPE="${SCOPE:-benchmark}"

echo "filling ${URL}/warm — scope=${SCOPE}, count=${COUNT} (single bulk request)"

# GNU date supports %s%N (nanoseconds since epoch); BSD date does not.
# Fall back to second-resolution if %N is unsupported.
if date +%N | grep -q '^[0-9]\{9\}$'; then
    NOW_NS() { date +%s%N; }
    NS_PER_MS=1000000
else
    NOW_NS() { echo $(($(date +%s) * 1000000000)); }
    NS_PER_MS=1000000
fi

start_ns=$(NOW_NS)

# Stream a single JSON document of the form:
#
#   {"items":[{"scope":"<S>","payload":{"i":1}}, ..., {"scope":"<S>","payload":{"i":N}}]}
#
# directly into curl's stdin via --data-binary @-. No temp file, no
# shell-loop overhead — awk emits the whole array in one pass.
{
    printf '{"items":['
    seq 1 "$COUNT" | awk -v scope="$SCOPE" '
        BEGIN { sep = "" }
        {
            printf "%s{\"scope\":\"%s\",\"payload\":{\"i\":%d}}", sep, scope, $1
            sep = ","
        }
    '
    printf ']}'
} | curl -fsS -X POST "$URL/warm" \
    -H 'Content-Type: application/json' \
    --data-binary @- \
    -o /dev/null

end_ns=$(NOW_NS)
elapsed_ms=$(( (end_ns - start_ns) / NS_PER_MS ))
[ "$elapsed_ms" -eq 0 ] && elapsed_ms=1
rate=$(( COUNT * 1000 / elapsed_ms ))

echo "done in ${elapsed_ms}ms (~${rate} items/s)"
echo
echo "verify with:"
echo "  curl ${URL}/stats"
echo "  curl '${URL}/head?scope=${SCOPE}&limit=3'"
