# scopecache as a FrankenPHP extension — prototype

This addon exposes scopecache's `*Gateway` directly to PHP code as a
native extension function, **bypassing HTTP entirely**.

## Why

When scopecache and PHP run together in a FrankenPHP+Caddy binary,
PHP→cache calls still go through HTTP today: libcurl → loopback TCP →
Caddy routing → scopecache handler → JSON encode/decode on both sides.
The harness measured a `~3.5 ms` floor for an `11–17 µs` cache
lookup — a `~200×` overhead ratio dominated entirely by transport.

A FrankenPHP Go extension compiles into the same binary as scopecache.
PHP calls become C `PHP_FUNCTION`s; the body lands in Go; the Go code
calls `*Gateway` methods in the same process. Order-of-magnitude
estimate for the speedup is `10×–50×`, but bench numbers come later
(see "Status" below).

## Status: one function, prototype

This is a deliberately minimal first step. The only PHP-callable
function exposed is:

```php
scopecache_get(string $scope, string $id): ?string
```

Hit returns the verbatim JSON payload as a PHP string; miss returns
`null`. No `scopecache_append`, `scopecache_tail`, `scopecache_render`,
no error reporting beyond null-on-miss, no scope/id validation surfaced
to PHP.

The extension pre-seeds one item in its init goroutine (`demo` /
`hello`) so `test.php` shows a working round-trip without first
needing an append-style function.

## Files

| file | role |
|---|---|
| `scopecache_ext.go` | the extension source (single Go file, `//export_php:function` directive picks up the signature) |
| `go.mod` | own module so the scopecache core stays stdlib-only |
| `build.sh` | compiles a `dist/frankenphp` binary with the extension baked in |
| `test.php` | minimal demo: one hit, two misses |

## Build

Requires Docker (the FrankenPHP builder image carries the PHP-ZTS
headers, `gen_stub.php`, and the Go toolchain — no host-level PHP
dev setup needed).

```bash
cd addons/frankenphp-ext
./build.sh
```

Cold build: `~5–15 min`. Warm rebuild: `~30–90 s`.

Output: `./dist/frankenphp` — a standard FrankenPHP binary that
additionally exposes `scopecache_get()` to PHP.

## Run the demo

```bash
cd addons/frankenphp-ext
./dist/frankenphp php-server -listen :8080 -root .
# in another shell:
curl http://localhost:8080/test.php
```

Expected output (`text/plain`):

```
=== scopecache PHP extension — single-function prototype ===

scopecache_get('demo', 'hello') -> string(54) "{"greeting":"hi from scopecache, via cgo, in-process"}"
scopecache_get('demo', 'no-such-item') -> NULL
scopecache_get('no-such-scope', 'hello') -> NULL

If the first call returned a JSON-shaped string and the other two
returned NULL, the extension is wired correctly.
```

## Open design questions (deferred)

These are the things to revisit once the prototype proves the
approach works:

1. **Shared Gateway with the Caddy module.** Today this addon
   instantiates its own `*Gateway`. A real deployment wants PHP and
   HTTP clients to hit the **same** cache — that requires a process-
   wide singleton that the `caddymodule/` `Provision` registers
   into and this extension reads from. Small refactor; defer until
   the API surface here is settled.

2. **Surfacing validation errors.** Currently invalid scope/id sizes
   or empty payloads silently return null (miss-shape). PHP idiom
   would prefer exceptions for genuinely-invalid input vs `null` for
   miss. Needs `frankenphp.ThrowException` (or equivalent) in the
   Go code.

3. **Append / tail / counter.** Once `_get` is verified, add the
   write-side functions:
   - `scopecache_append(string $scope, ?string $id, string $payload): array`
   - `scopecache_tail(string $scope, int $limit = 10): array`
   - `scopecache_counter_add(string $scope, string $id, int $by): int`

4. **Config plumbing.** Hard-coded caps in the prototype; production
   needs env-var or Caddyfile config (preferably the same knobs the
   standalone binary reads).

## Boundary discipline (per `CLAUDE.md`)

- This is an **addon**, not a core change.
- The scopecache core (`package scopecache`) stays stdlib-only.
- `*Gateway` is the only public surface this addon uses.
- Nothing in the core has been adjusted to make this possible — the
  Gateway API was already shaped for exactly this kind of in-process
  consumer.

## Reference

FrankenPHP extension API: <https://frankenphp.dev/docs/extensions/>
