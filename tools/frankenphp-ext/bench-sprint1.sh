#!/usr/bin/env bash
# bench-sprint1.sh — runs bench-sprint1.php against dist/frankenphp.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/dist/frankenphp"
HOST_PORT="${BENCH_PORT:-18084}"
RUNTIME_IMAGE="${RUNTIME_IMAGE:-dunglas/frankenphp:1.12-php8}"
CONTAINER_NAME="scopecache-bench-sprint1"
ITER="${ITER:-100000}"
WARMUP="${WARMUP:-5000}"
TAIL_LIMIT="${TAIL_LIMIT:-10}"

if [ ! -f "$BIN" ]; then
    echo "bench-sprint1: $BIN not found — run ./build.sh first" >&2
    exit 1
fi

cleanup() { docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

MSYS_NO_PATHCONV=1 docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -v "$SCRIPT_DIR:/app:ro" \
    -p "$HOST_PORT:8080" \
    --entrypoint /app/dist/frankenphp \
    "$RUNTIME_IMAGE" \
    run --config /app/Caddyfile.bench --adapter caddyfile >/dev/null

for i in $(seq 1 100); do
    if curl -sf --max-time 1 "http://127.0.0.1:$HOST_PORT/stats" >/dev/null 2>&1; then break; fi
    sleep 0.1
done

curl -sf --max-time 120 \
    "http://127.0.0.1:$HOST_PORT/bench-sprint1.php?iter=$ITER&warmup=$WARMUP&tail_limit=$TAIL_LIMIT"
