#!/usr/bin/env bash
# build.sh — produce a fully-static FrankenPHP + scopecache binary using
# the official static-builder-musl pipeline.
#
# Output: examples/frankenphp-bin/frankenphp-static-linux-<arch>
# Runs on any modern x86_64 / arm64 Linux without external dependencies
# (musl-linked, no glibc, no shared libraries).
#
# Time : ~15-45 min cold (compiles PHP + every extension from source).
# Disk : build pulls ~5-10 GB of intermediate docker layers.
#
# Usage:
#   ./build.sh                            # default: v1.12.2, default extensions
#   FRANKENPHP_VERSION=v1.12.2 ./build.sh
#   PHP_EXTENSIONS=curl,opcache ./build.sh
#   GITHUB_TOKEN=ghp_... ./build.sh       # avoids github API rate limits in spc
#
# Build-chain pitfalls (full rationale in README.md):
#   - Two scopecache --with flags (Go ignores `replace` directives in deps).
#   - Absolute --with paths (xcaddy runs from buildroot/bin in static mode).
#   - FRANKENPHP_VERSION must match the cloned tag.
#   - Platform must be pinned (bake defaults to amd64+arm64 parallel).
#   - GITHUB_TOKEN advised to avoid rate-limits mid-build.

set -euo pipefail

FRANKENPHP_VERSION="${FRANKENPHP_VERSION:-v1.12.2}"
PHP_EXTENSIONS="${PHP_EXTENSIONS:-curl,opcache,openssl,mbstring,sodium,pdo,pdo_sqlite,session,tokenizer,filter,ctype,iconv}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DIST_DIR="$REPO_ROOT/examples/frankenphp-bin"

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

mkdir -p "$DIST_DIR"

echo ">>> Cloning php/frankenphp@$FRANKENPHP_VERSION..."
git clone --depth=1 --branch "$FRANKENPHP_VERSION" \
    https://github.com/php/frankenphp "$WORK_DIR/fp" 2>&1 | tail -3

echo ">>> Copying scopecache source into build context (lands at /go/src/app/scopecache)..."
SCOPECACHE_DIR="$WORK_DIR/fp/scopecache"
mkdir -p "$SCOPECACHE_DIR"
cp "$REPO_ROOT/go.mod" "$SCOPECACHE_DIR/"
cp "$REPO_ROOT"/*.go "$SCOPECACHE_DIR/"
cp -r "$REPO_ROOT/cmd" "$SCOPECACHE_DIR/"
cp -r "$REPO_ROOT/caddymodule" "$SCOPECACHE_DIR/"
cp -r "$REPO_ROOT/addons" "$SCOPECACHE_DIR/"

echo ">>> Copying embed/ (Caddyfile + PHP app) into build context..."
cp -r "$SCRIPT_DIR/embed" "$WORK_DIR/fp/embed"

cd "$WORK_DIR/fp"

# Absolute --with paths: xcaddy runs from .../buildroot/bin during the
# static build, so relative paths like ./scopecache do not resolve.
XCADDY_ARGS="--with github.com/VeloxCoding/scopecache=/go/src/app/scopecache --with github.com/VeloxCoding/scopecache/caddymodule=/go/src/app/scopecache/caddymodule"

if [ -n "${GITHUB_TOKEN:-}" ]; then
    echo ">>> GITHUB_TOKEN is set — using it for github API calls inside spc."
else
    echo ">>> No GITHUB_TOKEN — build may rate-limit on github.com (set the env var if so)."
    export GITHUB_TOKEN=""
fi

echo ">>> Running docker buildx bake static-builder-musl (slow part — grab koffie)..."
# FRANKENPHP_VERSION must match the cloned tag — the bake file defaults to
# "dev" which makes the in-container build-static.sh try `git checkout dev`,
# which our shallow clone does not have.
# platform pinned to host arch — bake's default is amd64+arm64 parallel.
HOST_ARCH=$(uname -m)
case "$HOST_ARCH" in
    x86_64)        BAKE_PLATFORM="linux/amd64" ;;
    aarch64|arm64) BAKE_PLATFORM="linux/arm64" ;;
    *) echo "Unsupported arch: $HOST_ARCH"; exit 1 ;;
esac

docker buildx bake --load \
    --set "static-builder-musl.platform=$BAKE_PLATFORM" \
    --set "static-builder-musl.args.FRANKENPHP_VERSION=$FRANKENPHP_VERSION" \
    --set "static-builder-musl.args.XCADDY_ARGS=$XCADDY_ARGS" \
    --set "static-builder-musl.args.PHP_EXTENSIONS=$PHP_EXTENSIONS" \
    --set "static-builder-musl.args.EMBED=./embed" \
    static-builder-musl

echo ">>> Build finished — extracting binary from image..."
ARCH=$(uname -m)
CONTAINER_ID=$(docker create dunglas/frankenphp:static-builder-musl)
OUT="$DIST_DIR/frankenphp-static-linux-$ARCH"
docker cp "$CONTAINER_ID:/go/src/app/dist/frankenphp-linux-$ARCH" "$OUT"
docker rm "$CONTAINER_ID" >/dev/null
chmod +x "$OUT"

echo ""
echo ">>> Done."
ls -lh "$OUT"
echo ""
echo "    Linked modules:"
file "$OUT" | sed 's/^/    /'
echo ""
echo "    Run instructions: examples/frankenphp-bin/README.md"
