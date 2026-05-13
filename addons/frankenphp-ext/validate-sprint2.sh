#!/usr/bin/env bash
# validate-sprint2.sh — runs validate-sprint2.php against the current
# dist/frankenphp and reports PASS/FAIL aggregate.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/dist/frankenphp"
HOST_PORT="${VALIDATE_PORT:-18085}"
RUNTIME_IMAGE="${RUNTIME_IMAGE:-dunglas/frankenphp:1.12-php8}"
CONTAINER_NAME="scopecache-validate-sprint2"

if [ ! -f "$BIN" ]; then
    echo "validate-sprint2: $BIN not found — run ./build.sh first" >&2
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

OUT="$(curl -sf --max-time 30 "http://127.0.0.1:$HOST_PORT/validate-sprint2.php" || echo 'curl FAIL')"
echo "$OUT"

if echo "$OUT" | grep -q "^OVERALL: PASS$"; then
    exit 0
else
    exit 1
fi
