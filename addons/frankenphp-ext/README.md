# scopecache as a FrankenPHP extension

Exposes scopecache's `*Gateway` directly to PHP code as a native
extension — bypasses HTTP / cURL / JSON encoding on the PHP→cache hop
when scopecache and PHP run together in the same FrankenPHP binary.

Generic FrankenPHP-extension-build infrastructure (the cached
generator image, the catalogue of `gofmt`/`RETURN_NULL`/cgo pitfalls)
lives in [`tools/frankenphp-ext-builder/`](../../tools/frankenphp-ext-builder/).
This directory holds only the scopecache-specific extension code,
build wrapper, and test/bench harnesses.

## What's exposed

19 PHP functions, each mirroring an HTTP endpoint on `*Gateway`:

| PHP signature | Maps to |
|---|---|
| `scopecache_get(scope, id): ?array` | `GET /get` |
| `scopecache_get_by_seq(scope, seq): ?array` | `GET /get?seq=` |
| `scopecache_head(scope, after_seq, limit): ?array` | `GET /head` |
| `scopecache_tail(scope, limit): ?array` | `GET /tail` |
| `scopecache_render_by_id(scope, id): ?string` | `GET /render` (raw bytes) |
| `scopecache_render_by_seq(scope, seq): ?string` | `GET /render?seq=` (raw bytes) |
| `scopecache_append(scope, id, payload): ?array` | `POST /append` |
| `scopecache_upsert(scope, id, payload): ?array` | `POST /upsert` |
| `scopecache_update(scope, id, payload): ?array` | `POST /update` |
| `scopecache_counter_add(scope, id, by): ?array` | `POST /counter_add` |
| `scopecache_delete(scope, id): ?array` | `POST /delete` |
| `scopecache_delete_by_seq(scope, seq): ?array` | `POST /delete?seq=` |
| `scopecache_delete_up_to(scope, max_seq): ?array` | `POST /delete_up_to` |
| `scopecache_delete_scope(scope): ?array` | `POST /delete_scope` |
| `scopecache_wipe(): ?array` | `POST /wipe` |
| `scopecache_stats(): ?array` | `GET /stats` |
| `scopecache_scopelist(prefix, after, limit): ?array` | `GET /scopelist` |
| `scopecache_warm(grouped): ?array` | `POST /warm` |
| `scopecache_rebuild(grouped): ?array` | `POST /rebuild` |

Every `?array` function returns the HTTP success envelope as a
PHP-array (see RFC §6). Payloads are pre-decoded the way
`json_decode($body, true)` would decode them — `{"v":1}` arrives as
`['v' => 1]`, not as a raw JSON string. A `nil` return crosses to PHP
as `null` and means "no caddymodule loaded" (Provision never ran);
operator errors come back as `['ok' => false, 'error' => '...']`.

## Why

Loopback HTTP to a scopecache running in the same FrankenPHP binary
pays ~3.5 ms of transport cost for an 11-17 µs cache lookup —
~200× overhead. This extension compiles into the same binary, so
PHP→cache calls hit `*Gateway` methods directly through cgo. The
bench harness ([`bench-sprint1.sh`](bench-sprint1.sh)) measures
`scopecache_get` at ~640 ns / `~1.6 M qps` for a 54-byte payload;
the in-process route is roughly 1000× cheaper than the same call
over loopback HTTP.

PHP and the Caddy module share **one** `*Gateway` via the
process-wide registry in `gateway_registry.go` — the caddymodule
registers under `"default"` during Provision; this extension's
`defaultSlot` reads the same pointer. No second hidden cache.

## Files

| file | role |
|---|---|
| [`scopecache_ext.go`](scopecache_ext.go) | Extension source — one Go file holding all `//export_php:function` directives + cgo helpers + hand-rolled JSON-to-zval decoder |
| [`go.mod`](go.mod) | Module pin (with a `replace` directive against the in-repo scopecache source during local builds) |
| [`build.sh`](build.sh) | Two-stage Docker build that drives [`tools/frankenphp-ext-builder/`](../../tools/frankenphp-ext-builder/) |
| [`Caddyfile.bench`](Caddyfile.bench) | Caddyfile for the bench/test runtime (has the `scopecache {}` block that triggers `Provision`) |
| [`test.php`](test.php) + [`smoke.sh`](smoke.sh) | 8-check post-build sanity (8/8 must pass) |
| [`validate-sprint1.php`](validate-sprint1.php) + [`.sh`](validate-sprint1.sh) | 42-check correctness harness — append/tail edge cases |
| [`validate-sprint2.php`](validate-sprint2.php) + [`.sh`](validate-sprint2.sh) | 136-check correctness harness — every other endpoint + warm/rebuild byte-round-trip |
| [`bench-sprint1.php`](bench-sprint1.php) + [`.sh`](bench-sprint1.sh) | Single-thread per-call bench for get/tail/append |
| [`bench-ext-only.php`](bench-ext-only.php) + [`payload-sweep.sh`](payload-sweep.sh) | Payload-size sweep |
| [`bench.php`](bench.php) + [`compare.sh`](compare.sh) | 5-path comparison: cgo vs HTTP vs Redis vs Memcached |
| `dist/` (gitignored) | Build output — `./dist/frankenphp` is a FrankenPHP binary with the scopecache extension baked in |

## Build

Requires Docker. Cold build ~5–15 min, warm rebuild ~1–3 min.

```bash
cd addons/frankenphp-ext
./build.sh
```

Force-rebuild of the cached generator image (e.g. when bumping the
FrankenPHP base tag):

```bash
./build.sh --rebuild-gen-image
```

## Validate after a build

```bash
./smoke.sh             #  8/8 sanity
./validate-sprint1.sh  # 42/42 append/tail edges
./validate-sprint2.sh  # 136/136 full surface + byte-exact round-trip
./bench-sprint1.sh     # per-call latency + throughput
```

## Demo

```bash
docker run --rm -v "$PWD:/app:ro" -p 8080:8080 \
    --entrypoint /app/dist/frankenphp \
    dunglas/frankenphp:1.12-php8 \
    run --config /app/Caddyfile.bench --adapter caddyfile

curl http://localhost:8080/test.php
```

`test.php` prints the envelope for each call; `var_dump` lines show
the array shape (key set + decoded payload).

## Boundary discipline

- This is an **addon**. The scopecache core (`package scopecache`)
  stays stdlib-only and does not import anything from here.
- The only public surface consumed is `*Gateway` and the typed
  response structs in [`response_types.go`](../../response_types.go).
- The on-the-wire shape of every PHP-array return mirrors the HTTP
  envelope in RFC §6 — single source of truth, no parallel spec.
