#!/usr/bin/env bash
# compare.sh — PHP→Redis vs PHP→Memcached vs PHP→scopecache (cgo) GET cost.
#
# Five paths × two payload sizes (54 B + 5 KiB):
#   - Redis cold      (Redis::connect + get + close per iter)
#   - Redis warm      (one Redis::connect, get N times)
#   - Memcached cold  (new Memcached + addServer per iter)
#   - Memcached warm  (one Memcached, get N times)
#   - scopecache cgo  (in-process, no connection)
#
# Builds (or reuses) a runtime image with phpredis + memcached added
# to stock FrankenPHP (Dockerfile.bench), starts Redis + Memcached
# side-containers on a private Docker network, runs compare.php
# against all three backends, cleans up.
#
# Per-knob env-var overrides:
#   COMPARE_PORT   host port to expose FrankenPHP on (default 18086)
#   ITER_COLD      iters per cold path (default 2000)
#   ITER_WARM      iters per warm Redis/Memcached path (default 20000)
#   ITER_LOCAL     iters for scopecache cgo (default 100000)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/dist/frankenphp"
HOST_PORT="${COMPARE_PORT:-18086}"
NET="frankenphp-ext-compare-net"

REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
MEMCACHED_IMAGE="${MEMCACHED_IMAGE:-memcached:1.6-alpine}"
BENCH_IMAGE="${BENCH_IMAGE:-frankenphp-ext-bench:local}"

REDIS_C="frankenphp-ext-compare-redis"
MEMC_C="frankenphp-ext-compare-memcached"
APP_C="frankenphp-ext-compare-app"

ITER_COLD="${ITER_COLD:-2000}"
ITER_WARM="${ITER_WARM:-20000}"
ITER_LOCAL="${ITER_LOCAL:-100000}"

if [ ! -f "$BIN" ]; then
    echo "compare: $BIN not found — run ./build.sh first" >&2
    exit 1
fi

cleanup() {
    docker rm -f "$REDIS_C" "$MEMC_C" "$APP_C" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

# Build the bench runtime image once. Dockerfile.bench adds phpredis
# + memcached on top of dunglas/frankenphp:1.12-php8.
if ! docker image inspect "$BENCH_IMAGE" >/dev/null 2>&1; then
    echo ">>> building $BENCH_IMAGE (one-time, ~30-60 s)"
    # cd + "." context: avoids Git-Bash rewriting absolute /e/... paths
    # before the Windows docker daemon sees them.
    ( cd "$SCRIPT_DIR" && MSYS_NO_PATHCONV=1 docker build \
        -t "$BENCH_IMAGE" \
        -f Dockerfile.bench \
        . >/dev/null )
fi

docker network create "$NET" >/dev/null

MSYS_NO_PATHCONV=1 docker run -d --rm \
    --network "$NET" --name "$REDIS_C" \
    "$REDIS_IMAGE" >/dev/null

MSYS_NO_PATHCONV=1 docker run -d --rm \
    --network "$NET" --name "$MEMC_C" \
    "$MEMCACHED_IMAGE" >/dev/null

MSYS_NO_PATHCONV=1 docker run -d --rm \
    --network "$NET" \
    --name "$APP_C" \
    -v "$SCRIPT_DIR:/app:ro" \
    -p "$HOST_PORT:8080" \
    --entrypoint /app/dist/frankenphp \
    "$BENCH_IMAGE" \
    run --config /app/Caddyfile.bench --adapter caddyfile >/dev/null

# Wait for FrankenPHP /stats to answer.
for i in $(seq 1 200); do
    if curl -sf --max-time 1 "http://127.0.0.1:$HOST_PORT/stats" >/dev/null 2>&1; then break; fi
    sleep 0.1
done

# Wait for Redis to accept connections.
for i in $(seq 1 50); do
    if docker exec "$REDIS_C" redis-cli ping 2>/dev/null | grep -q PONG; then break; fi
    sleep 0.1
done

curl -sf --max-time 600 \
    "http://127.0.0.1:$HOST_PORT/compare.php?iter_cold=$ITER_COLD&iter_warm=$ITER_WARM&iter_local=$ITER_LOCAL"
