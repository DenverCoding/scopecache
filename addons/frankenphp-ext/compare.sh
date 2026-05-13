#!/usr/bin/env bash
# compare.sh — head-to-head bench for two scopecache extension binaries
# in this dist/ directory. Use to quantify cgo-side micro-optimisations
# like zero-copy unsafe.String / direct zend_string_init.
#
# Expects:
#   - dist/frankenphp.old   (baseline)
#   - dist/frankenphp       (candidate, e.g. the just-built optimised one)
#
# Runs the same bench-ext-only.php against each binary in a fresh
# container, prints the two reports side-by-side.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST_PORT="${COMPARE_PORT:-18081}"
RUNTIME_IMAGE="${RUNTIME_IMAGE:-dunglas/frankenphp:1.12-php8}"
ITER="${ITER:-200000}"
WARMUP="${WARMUP:-5000}"

run_one() {
    local label="$1" binary_in_container="$2"
    # Container names must be [a-zA-Z0-9_.-] — strip spaces and other
    # non-alphanumerics from the label for the cname only; display label
    # stays human-readable.
    local cname_label="${label//[^A-Za-z0-9_.-]/_}"
    local cname="scopecache-cmp-$$-${cname_label}"
    docker rm -f "$cname" >/dev/null 2>&1 || true

    MSYS_NO_PATHCONV=1 docker run -d --rm \
        --name "$cname" \
        -v "$SCRIPT_DIR:/app:ro" \
        -p "$HOST_PORT:8080" \
        --entrypoint "$binary_in_container" \
        "$RUNTIME_IMAGE" \
        run --config /app/Caddyfile.bench --adapter caddyfile >/dev/null

    # Wait until ready.
    for i in $(seq 1 100); do
        if curl -sf --max-time 1 "http://127.0.0.1:$HOST_PORT/stats" >/dev/null 2>&1; then
            break
        fi
        sleep 0.1
    done

    echo "==== $label ($binary_in_container) ===="
    curl -sf --max-time 60 "http://127.0.0.1:$HOST_PORT/bench-ext-only.php?iter=$ITER&warmup=$WARMUP"
    echo

    docker rm -f "$cname" >/dev/null 2>&1 || true
}

# Run all binaries that exist; labels reflect what each represents in
# the optimisation timeline. Skip with a note if a binary isn't present.
# Format: "label|/in-container/path"
for entry in \
    "OLD      |/app/dist/frankenphp.old" \
    "OPT 1+2  |/app/dist/frankenphp.opt" \
    "OPT 1+2+3|/app/dist/frankenphp"
do
    label="${entry%%|*}"
    bin="${entry##*|}"
    host_path="$SCRIPT_DIR${bin#/app}"
    if [ ! -f "$host_path" ]; then
        echo "==== $label ($bin) ===="
        echo "  skipped: binary not found at $host_path"
        echo
        continue
    fi
    run_one "$label" "$bin"
done
