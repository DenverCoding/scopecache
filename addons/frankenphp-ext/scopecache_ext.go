// scopecache_ext.go — FrankenPHP extension that exposes scopecache's
// Go-level *Gateway directly to PHP.
//
// Why this exists: even when scopecache runs inside the same
// FrankenPHP/Caddy binary as the PHP app, PHP→cache calls today still
// go through HTTP (libcurl → loopback TCP → Caddy routing → scopecache
// handler → JSON encode/decode on both sides). The harness measured
// that loopback-HTTP floor at ~3.5 ms for an 11-17 µs cache lookup —
// a ~200× overhead ratio dominated entirely by transport.
//
// This extension removes that transport layer. PHP calls a function
// (scopecache_get, etc.); the FrankenPHP extension generator turns
// that into a C-level PHP_FUNCTION whose body lands in this Go file;
// the Go function calls *Gateway methods in the same process; the
// return value crosses back through cgo as a native PHP value.
//
// Measured wins (54-byte payload, FrankenPHP 8.5 ZTS):
//   - ~750× vs PHP→scopecache over loopback HTTP (persistent curl handle)
//   - ~400× vs PHP→phpredis→Redis (persistent connection)
//   - ~1900× vs PHP→phpredis→Redis (fresh connection per call)
//
// See addons/frankenphp-ext/bench.php for the measurement harness.
//
// Boundary discipline (CLAUDE.md):
//   - This is an addon. The core never changes shape to accommodate it.
//   - PHP and the Caddy module share one *Gateway via the process-wide
//     named registry in gateway_registry.go: the caddymodule registers
//     under "default" during Provision(), this extension looks it up at
//     every call. Same data both ways; same Caddyfile config; same
//     /stats output. No second hidden cache.
//
// Hot-path discipline:
//   - The Gateway pointer is looked up once at package init via
//     scopecache.LookupGatewaySlot, then read per call with a single
//     atomic Load (~1-2 ns) instead of going through the registry's
//     RLock + map lookup (~30-40 ns). Caddy reload swaps the slot
//     contents atomically — no invalidation logic needed here.
//   - For READ-ONLY paths (scopecache_get, scopecache_tail), scope/id
//     cross C→Go as zero-copy unsafe.String views into the zend_string
//     bytes (zendStringView). Safe because the Gateway consumes them
//     synchronously and retains no references — see store.get and
//     buffer_read.getByID.
//   - For WRITE paths (scopecache_append), scope/id MUST be copied
//     (zendStringCopy) because they become permanent map keys inside
//     scopecache. The zero-copy alias would point to PHP-arena memory
//     that PHP frees at request end, leaving the map indexed by
//     garbage.
//   - The returned payload skips the []byte→string→zend_string detour
//     that frankenphp.PHPString takes, directly emalloc'ing a fresh
//     zend_string in the PHP request arena via phpStringFromBytes.
//   - Input payloads on the write path use zendStringBytes (alias);
//     safe because Gateway.Append's cloneItemPayload copies them
//     synchronously at the boundary.

package scopecache_ext

// #include <Zend/zend_string.h>
import "C"

import (
	"sync/atomic"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
	"github.com/dunglas/frankenphp"
)

// defaultSlot is the atomic *Gateway slot for the "default" name,
// cached at package init. LookupGatewaySlot lazily creates the slot
// on first reference, so this is safe even if package init runs
// before the caddymodule's Provision() — the slot holds nil until
// Register fires, and our scopecache_get already handles that case.
var defaultSlot *atomic.Pointer[scopecache.Gateway] = scopecache.LookupGatewaySlot("default")

// zendStringView returns a Go string aliasing the zend_string's byte
// storage — zero copies, zero allocations. Valid only for the
// duration of the calling PHP_FUNCTION: the underlying zend_string is
// PHP's emalloc'd request memory and lives at least as long as the
// request, but callers must NOT retain the returned string past their
// own cgo-call return.
//
// scopecache.GetByID satisfies this constraint — scope/id are used as
// synchronous map keys and never stored. Cross-checked in
// [store.go](../../store.go) and [buffer_read.go](../../buffer_read.go).
func zendStringView(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// zendStringBytes returns a []byte view over a zend_string, no copy.
// Same lifetime contract as zendStringView. Used for payload bytes
// that scopecache.Append immediately clones at the Gateway boundary,
// so retention by scopecache is not an issue.
func zendStringBytes(s *C.zend_string) []byte {
	if s == nil {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// zendStringCopy returns a Go string with a fresh copy of the
// zend_string bytes. Safe for permanent retention — use this when
// the resulting string will outlive the calling PHP_FUNCTION (e.g.
// becomes a map key in scopecache's internal structures).
//
// scopecache_append needs this for both scope and id because those
// strings become permanent map keys: s.shards[X].scopes[scope] and
// b.byID[id]. zendStringView aliases would point to PHP-arena
// memory that PHP frees once the request ends, leaving the map
// indexed by garbage. scopecache_get / scopecache_tail can use
// zendStringView because they only LOOK UP, never STORE.
func zendStringCopy(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(&s.val)), C.int(s.len))
}

// phpStringFromBytes emalloc's a fresh zend_string in the PHP request
// arena and copies the given bytes in, skipping the []byte→string→
// zend_string detour frankenphp.PHPString takes. The returned pointer
// is owned by the wrapper's RETURN_STR — PHP frees it on request
// shutdown.
//
// Empty input returns nil; combined with our build-time wrapper
// patch (RETURN_EMPTY_STRING→RETURN_NULL), PHP sees null. NB: this
// means an item legitimately holding a 0-byte payload would be
// reported as a miss. Safe today because scopecache validation
// requires non-empty JSON; documented here so a future validation
// loosening upstream doesn't silently break this extension.
func phpStringFromBytes(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(C.zend_string_init(
		(*C.char)(unsafe.Pointer(&b[0])),
		C.size_t(len(b)),
		C._Bool(false),
	))
}

// scopecache_get looks up a single item by scope + id and returns its
// payload bytes as a PHP string. Misses return null.
//
// Wire shape (PHP-visible):
//
//	scopecache_get(string $scope, string $id): ?string
//
// Returned bytes are the verbatim JSON payload — the same bytes
// served by GET /get?scope=X&id=Y under the "item.payload" key in
// the HTTP envelope, just without the envelope.
//
// Empty/whitespace inputs and over-cap scope/id strings are not
// validated here; the *Gateway layer enforces shape rules and a
// future revision will surface validation errors as PHP exceptions.
// For the prototype, invalid input simply returns null.
//
// Also returns null when no scopecache caddymodule is loaded in this
// binary (defaultSlot.Load() returns nil because no Provision ever
// stored a Gateway into it). An operator seeing only nulls should
// check that the Caddyfile has a `scopecache {}` block — without it,
// no Provision() ever ran, so no *Gateway is registered.
//
// export_php:function scopecache_get(string $scope, string $id): ?string
func scopecache_get(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	if !found {
		return nil
	}
	return phpStringFromBytes(item.Payload)
}

// scopecache_tail returns the newest `limit` items in scope, oldest-
// first within the window. Each array element is the verbatim JSON
// payload bytes of one item (same shape as scopecache_get returns).
//
// Wire shape (PHP-visible):
//
//	scopecache_tail(string $scope, int $limit): ?array
//
// Distinguishes "no scope" from "empty scope":
//   - no Caddy module loaded → null
//   - scope does not exist → null
//   - scope exists, zero items → []     (empty array, not null)
//   - scope exists, N items → [payload1, payload2, ...]
//
// The null-vs-empty-array distinction relies on the build-time
// wrapper patch (RETURN_EMPTY_ARRAY → RETURN_NULL): when Go returns
// a nil unsafe.Pointer, PHP sees null; when Go returns a non-nil
// PHPPackedArray (even of zero elements), PHP sees an array.
//
// Per-element cost is dominated by the PHPPackedArray helper's
// detour for each string ([]byte → string → zend_string_init). A
// future optimisation could build the array element-by-element with
// phpStringFromBytes-style helpers; for Sprint 1 the readability of
// PHPPackedArray wins.
//
// export_php:function scopecache_tail(string $scope, int $limit): ?array
func scopecache_tail(scope *C.zend_string, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	items, _, found := gw.Tail(zendStringView(scope), int(limit), 0)
	if !found {
		return nil
	}
	payloads := make([]any, len(items))
	for i, item := range items {
		payloads[i] = string(item.Payload)
	}
	return frankenphp.PHPPackedArray(payloads)
}

// scopecache_append inserts a new item with cache-assigned seq and
// returns the assigned seq. The payload bytes must be valid JSON
// (any JSON value — object, array, string, number, etc.); shape
// validation lives at the *Gateway boundary.
//
// Wire shape (PHP-visible):
//
//	scopecache_append(string $scope, string $id, string $payload): int
//
// Returns:
//   - seq (>= 1) on success
//   - 0 on any error: no Caddy module loaded, shape validation
//     failure, capacity exceeded, duplicate id within scope. seq=0
//     is never a real assigned seq (the counter starts at 1), so
//     this sentinel is unambiguous.
//
// Sprint-3 plan: replace the 0-sentinel with PHP exceptions so
// callers can distinguish error categories. For Sprint 1 the
// sentinel keeps the cgo signature simple.
//
// export_php:function scopecache_append(string $scope, string $id, string $payload): int
func scopecache_append(scope *C.zend_string, id *C.zend_string, payload *C.zend_string) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	// Scope and id MUST be copied — they become permanent map keys
	// inside scopecache (b.byID[id], s.shards[X].scopes[scope]). A
	// zero-copy unsafe.String alias would point to PHP-arena memory
	// that PHP frees at request end, leaving the map indexed by
	// garbage. Payload is fine to alias here because Gateway.Append's
	// cloneItemPayload copies it synchronously.
	item := scopecache.Item{
		Scope:   zendStringCopy(scope),
		ID:      zendStringCopy(id),
		Payload: zendStringBytes(payload),
	}
	result, err := gw.Append(item)
	if err != nil {
		return 0
	}
	return int64(result.Seq)
}
