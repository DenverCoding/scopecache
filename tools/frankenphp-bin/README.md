# FrankenPHP + scopecache static-binary builder

Produces a single fully-static Linux binary that bundles:

- **FrankenPHP** (Caddy web server + embedded PHP runtime)
- **scopecache** (this repo) as a Caddy module
- **An embedded PHP app + Caddyfile** ([`embed/`](embed/)) baked into
  the binary's filesystem

The output is musl-linked, has no glibc or shared-library dependencies,
and runs on any modern x86_64 / arm64 Linux machine — drop it on a VPS,
make it executable, run it. No package installs, no Docker required at
runtime.

```bash
./build.sh        # ~15-45 min cold
```

Output lands in [`examples/frankenphp-bin/`](../../examples/frankenphp-bin/);
the run-side README is there.

## What lives here

| file | role |
|---|---|
| [`build.sh`](build.sh) | Orchestrator: clones FrankenPHP, copies in the scopecache source + the `embed/` content, runs `docker buildx bake static-builder-musl`, extracts the binary. |
| [`embed/Caddyfile`](embed/Caddyfile) | The Caddyfile the binary uses at runtime. Baked in via `EMBED=./embed`. Override at runtime by passing `--config /path/to/your.Caddyfile`. |
| [`embed/public/index.php`](embed/public/index.php) | Default "hello world" PHP page baked into the binary. The runtime Caddyfile points `root` at this. |

The `embed/` content is the binary's default. You can override either
piece at runtime by serving from disk instead.

## Configurable inputs

```bash
FRANKENPHP_VERSION=v1.12.2 ./build.sh
PHP_EXTENSIONS=curl,opcache ./build.sh
GITHUB_TOKEN=ghp_... ./build.sh
```

| env var | default | purpose |
|---|---|---|
| `FRANKENPHP_VERSION` | `v1.12.2` | FrankenPHP tag to clone + bake against |
| `PHP_EXTENSIONS` | `curl,opcache,openssl,mbstring,sodium,pdo,pdo_sqlite,session,tokenizer,filter,ctype,iconv` | comma-separated list passed to spc |
| `GITHUB_TOKEN` | (unset) | passed to the static-php-cli pipeline to avoid github API rate-limits during the build |

## Build-chain pitfalls

These survived a build session that fell over on each one. The
workarounds are baked into [`build.sh`](build.sh); these notes explain
**why** each line is there.

### 1. xcaddy needs two `--with` flags for scopecache

xcaddy synthesises a top-level `go.mod` for the build. Go ignores
`replace` directives in dependency modules — so the `replace
github.com/VeloxCoding/scopecache => ../` inside `caddymodule/go.mod`
has no effect, and xcaddy fetches whatever the on-disk
`caddymodule/go.mod` pins (which may not match the local scopecache
source).

**Workaround:** pass `--with` for both root + caddymodule, pointing
each at the staged local source:

```
--with github.com/VeloxCoding/scopecache=/go/src/app/scopecache
--with github.com/VeloxCoding/scopecache/caddymodule=/go/src/app/scopecache/caddymodule
```

### 2. Absolute paths for `--with` (static build only)

In the static-builder-musl pipeline, xcaddy runs from
`.../buildroot/bin`. Relative paths like `./scopecache` resolve
against that directory and fail. Use absolute `/go/src/app/scopecache`
— which is where `build.sh` stages the source.

### 3. `FRANKENPHP_VERSION` must match the cloned tag

The default bake-file's in-container build script reads
`$FRANKENPHP_VERSION` and does `git checkout` on that ref. Default
is `"dev"` — but `build.sh` does a `--depth=1 --branch=vX.Y.Z` clone,
which only contains the tagged ref, not `dev`. Result: build fails
mid-pipeline trying to check out a missing ref.

**Workaround:** explicitly pass
`--set static-builder-musl.args.FRANKENPHP_VERSION=$FRANKENPHP_VERSION`
matching the cloned tag.

### 4. Platform must be pinned

`docker buildx bake` defaults to building both `linux/amd64` and
`linux/arm64` in parallel. On a single-architecture host (the typical
case) the cross-arch leg hangs forever waiting for binfmt
registration that isn't there.

**Workaround:** detect `uname -m`, set `BAKE_PLATFORM` to the host
arch, pass it as `--set static-builder-musl.platform=$BAKE_PLATFORM`.

### 5. `GITHUB_TOKEN` avoids mid-build rate-limits

The static-php-cli pipeline issues many anonymous github API calls
(downloading PHP, every extension's source, build deps). Without a
token, an anonymous IP hits the github API rate-limit somewhere
around minute 15 of a 30-minute build, and the build dies.

**Workaround:** export a `$GITHUB_TOKEN` env var before running.
A personal-access token with no special scopes is enough — it just
raises the rate-limit ceiling. `build.sh` reads `$GITHUB_TOKEN` and
forwards it.

### 6. xcaddy itself must be copied in separately

The FrankenPHP `1.12-builder-php8` image carries the Go toolchain
+ the FrankenPHP source, but **not** xcaddy. The static-builder bake
pipeline pulls xcaddy in via its own image-layer, so `build.sh`
itself doesn't need to handle this — but if you ever swap the static
pipeline for a plain `docker build` against
`dunglas/frankenphp:1.12-builder-php8` directly, you'll need:

```dockerfile
COPY --from=caddy:builder /usr/bin/xcaddy /usr/bin/xcaddy
```

### 7. FrankenPHP `1.12` pin

Earlier `:latest-builder` images shipped FrankenPHP source that
didn't match Caddy `v2.11.2` — xcaddy build fails with type-mismatch
errors in the Caddy glue layer. The `1.12-*` tags pin a working
combination.

If you bump `FRANKENPHP_VERSION`, also verify that the
`dunglas/frankenphp:<new>-builder-php8` image's Caddy glue is
compatible with the Caddy version xcaddy will pull. The compatibility
window is narrow — newer Caddy + older FrankenPHP glue (or vice
versa) breaks the build.

## Customising the embedded app

Edit [`embed/Caddyfile`](embed/Caddyfile) and
[`embed/public/index.php`](embed/public/index.php), then rerun
`build.sh`. The whole `embed/` tree gets baked into the binary's
internal filesystem at build time, so changes require a rebuild.

For runtime-only changes (no rebuild), serve from a real directory
and override at startup:

```bash
./frankenphp-static-linux-x86_64 run --config /etc/my.Caddyfile
```

…with a Caddyfile whose `root` points at a real directory on disk.

## Where the binary goes after building

`build.sh` writes to [`examples/frankenphp-bin/`](../../examples/frankenphp-bin/)
in the repo root. See that directory's README for run instructions.
