# FrankenPHP Go-extension builder

Reusable build infrastructure for FrankenPHP extensions written in Go
against the `//export_php:` directive. Currently consumed by
[`addons/frankenphp-ext/`](../../addons/frankenphp-ext/) (the scopecache
extension); the contents are deliberately scopecache-agnostic so a
second extension in this repo — or in a separate project that vendors
this directory — can reuse the same pipeline.

## What lives here

| file | role |
|---|---|
| [`Dockerfile.gen`](Dockerfile.gen) | Cached generator image — bakes in PHP-ZTS headers + `frankenphp-gen` (built from master so `extension-init` is present) + `xcaddy` + the right `GEN_STUB_SCRIPT` env path. ~3 min cold, instant warm. |
| [`Dockerfile.bench`](Dockerfile.bench) | Runtime image adding the `phpredis` and `memcached` C-extensions to stock FrankenPHP — for benches that compare the new extension against those backends. |
| [`README.md`](README.md) | This file — recipe + the catalogue of build-chain pitfalls. |

## Build recipe (two-stage)

```bash
# 1. (Re)build the generator image once.
docker build -t frankenphp-ext-builder:latest \
    -f tools/frankenphp-ext-builder/Dockerfile.gen \
    tools/frankenphp-ext-builder/

# 2. Run that image with the extension source bind-mounted. The
#    container does: stage source → `frankenphp-gen extension-init`
#    → patch the generated C wrappers → `xcaddy build` → emit the
#    final binary into /out (host: ./dist/).
docker run --rm \
    -v "$PWD:/scopecache:ro" \
    -v "$PWD/addons/your-ext:/ext-src:ro" \
    -v "$PWD/addons/your-ext/dist:/out" \
    -w /work \
    frankenphp-ext-builder:latest \
    bash -c '...'  # see addons/frankenphp-ext/build.sh for the live script
```

The live consumer is [`addons/frankenphp-ext/build.sh`](../../addons/frankenphp-ext/build.sh)
— it has the `xcaddy --with` arguments wired up for scopecache plus
the staging steps below.

## Build-chain pitfalls (why every sed-patch is there)

The build script does several non-obvious things. Each works around a
real bug that bit a build session at some point.

### 1. `// export_php:` (with space) on disk, `//export_php:` (tight) at build time

`frankenphp-gen extension-init` only matches the **tight** form
`//export_php:` (no space after `//`). But `gofmt` and most editor
"format-on-save" rules rewrite that into `// export_php:` (with space)
because it would otherwise look like an unparseable comment.

**Workaround:** keep `// export_php:` on disk (gofmt-clean, pre-commit
hooks happy); `sed` it back to `//export_php:` inside the build
container before invoking the generator.

```bash
sed -i 's|^// export_php:|//export_php:|g' /work/ext/your-ext.go
```

### 2. `RETURN_EMPTY_STRING` / `RETURN_EMPTY_ARRAY` instead of `RETURN_NULL` for `?string` / `?array`

The upstream extgen template emits `RETURN_EMPTY_STRING()` /
`RETURN_EMPTY_ARRAY()` when the Go function returns `nil`, regardless
of whether the directive declared the return type as nullable
(`?string` / `?array`). That collapses PHP `null` into `""` / `[]`,
breaking the idiomatic `if ($r === null)` miss check.

**Workaround:** post-process the generated C with sed:

```bash
sed -i -e 's|RETURN_EMPTY_STRING();|RETURN_NULL();|g' \
       -e 's|RETURN_EMPTY_ARRAY();|RETURN_NULL();|g' \
    /work/ext/build/*.c /work/ext/*.c
```

### 3. Apostrophes inside the outer `bash -c '...'`

`docker run ... bash -c '...'` uses single quotes outside. A single
apostrophe inside any of the heredoc-style comments closes the outer
string mid-script — error symptoms range from "permission denied" to
"unexpected token". Keep all `bash -c` body text apostrophe-free, or
escape rigorously.

### 4. Generator wants `int64`, not `C.zend_long`

PHP `int` parameters must surface as Go `int64` in the generated
function signature. If the source uses `C.zend_long` instead, the
generator silently skips the function (no error, the symbol just
never reaches PHP). Always declare PHP-`int` params as Go `int64`.

### 5. cgo macros need static-inline trampolines

PHP's `ZVAL_*` and `zend_new_array` macros cannot be invoked through
cgo (cgo only sees functions). Each macro you want from Go needs a
small static-inline C wrapper in the cgo preamble:

```c
static inline void sc_zval_str(zval *zv, zend_string *s) { ZVAL_STR(zv, s); }
static inline zend_array *sc_zend_new_array(uint32_t size) { return zend_new_array(size); }
```

### 6. `MSYS_NO_PATHCONV=1` on Windows / Git-Bash hosts

Git-Bash on Windows rewrites absolute Linux-style paths like
`/scopecache` into Windows drive paths (`C:\scopecache`) before
they reach the docker daemon. Prefix every `docker build` and
`docker run` with `MSYS_NO_PATHCONV=1` to stop that.

### 7. xcaddy build flags for cgo

```bash
CGO_ENABLED=1 \
XCADDY_GO_BUILD_FLAGS="-ldflags=-linkmode=external" \
CGO_CFLAGS="-D_GNU_SOURCE $(php-config --includes)" \
CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" \
    xcaddy build ...
```

- `-D_GNU_SOURCE` is needed because PHP-Zend headers reference
  `memrchr()` (a GNU extension).
- `-linkmode=external` is needed because Go 1.26's internal linker
  chokes when stitching multiple cgo packages
  (FrankenPHP + your extension) into one binary.

### 8. UAF on string map-keys: write paths must copy

`unsafe.String((*byte)(unsafe.Pointer(&s.val)), int(s.len))` produces a
zero-copy alias over PHP's emalloc'd `zend_string` bytes. That's
safe for **synchronous read paths** (the cache uses the key, returns,
PHP frees the zend_string at request end). It is **not** safe when
the string becomes a permanent map key — those aliases point at
freed PHP memory after the request ends, indexing the map by garbage.

For write paths that retain keys, deep-copy via `C.GoStringN` instead:

```go
func zendStringCopy(s *C.zend_string) string {
    return C.GoStringN((*C.char)(unsafe.Pointer(&s.val)), C.int(s.len))
}
```

The scopecache extension does exactly this distinction — `scopecache_get`
uses the alias, `scopecache_append` uses the copy.

## Versioning

Both Dockerfiles pin `dunglas/frankenphp:1.12-*`. Bump the tag here
when you want a newer FrankenPHP / PHP / Go combination, then
`./build.sh --rebuild-gen-image` in the consuming addon.

## Adding a second extension

1. Create `addons/<your-ext>/` with `your-ext.go` (use
   `//export_php:function` directives) and a `go.mod` that imports the
   scopecache `*Gateway` (if needed).
2. Copy [`addons/frankenphp-ext/build.sh`](../../addons/frankenphp-ext/build.sh)
   and adjust the `xcaddy --with` lines to point at your packages.
3. Both extensions can register on the same `frankenphp-ext-builder`
   image — no per-extension image needed.
