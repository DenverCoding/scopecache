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

/*
#include <php.h>

// Inline cgo-visible wrappers around PHP's ZVAL_* macros and a few
// utility helpers. cgo cannot call C macros directly, so each macro
// gets a tiny static-inline trampoline. Same pattern frankenphp's
// types.go uses internally (their __zval_*__ helpers), kept under
// our own naming so it is clear at the call site that the symbol
// lives in this addon, not in upstream frankenphp. <php.h> is the
// master include that pulls in zend_types.h (zval, ZVAL_*),
// zend_string.h (zend_string_init), zend_hash.h
// (zend_hash_next_index_insert, zend_new_array) and everything
// else PHP-API-side.
static inline void sc_zval_str(zval *zv, zend_string *s) {
    ZVAL_STR(zv, s);
}
static inline void sc_zval_empty_string(zval *zv) {
    ZVAL_EMPTY_STRING(zv);
}
// zend_new_array itself is a macro expanding to _zend_new_array(size);
// cgo cannot call macros, so trampoline it.
static inline zend_array *sc_zend_new_array(uint32_t size) {
    return zend_new_array(size);
}
*/
import "C"

import (
	"encoding/json"
	"sync/atomic"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
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

// phpPackedArrayFromItems builds a packed PHP zend_array where each
// element is a zend_string constructed directly from the corresponding
// scopecache.Item.Payload bytes. Bypasses frankenphp.PHPPackedArray
// to drop per-element overhead:
//
//   - no []any boxing of each element (1 interface-alloc avoided)
//   - no string(payload) conversion (1 byte-copy avoided)
//   - no per-element zval emalloc + efree (2 cgo calls + alloc avoided)
//   - no type-switch dispatch in phpValue
//
// One stack-allocated zval is reused across iterations — zend_hash
// copies the zval contents on insert, so reuse is safe. The
// zend_strings carry refcount=1 owned by the array slot; the
// wrapper's RETURN_ARR transfers ownership to PHP, which drops the
// reference at request end.
//
// Per-element cost target: ~100 ns/elt vs the ~200 ns of the
// generic frankenphp.PHPPackedArray path. See pitfall #N in the
// addon notes (CLAUDE_PHPEXTENSION_IN_GO.md) for the design
// rationale and the projected curve.
//
// Returns nil ONLY for a nil input slice — an EMPTY slice still
// returns a non-nil zend_array so the wrapper emits an empty PHP
// array (`[]`), distinguishing it from PHP `null` for callers.
func phpPackedArrayFromItems(items []scopecache.Item) unsafe.Pointer {
	if items == nil {
		return nil
	}
	arr := C.sc_zend_new_array(C.uint32_t(len(items)))
	var zv C.zval
	for i := range items {
		payload := items[i].Payload
		if len(payload) == 0 {
			C.sc_zval_empty_string(&zv)
		} else {
			zs := C.zend_string_init(
				(*C.char)(unsafe.Pointer(&payload[0])),
				C.size_t(len(payload)),
				C._Bool(false),
			)
			C.sc_zval_str(&zv, zs)
		}
		C.zend_hash_next_index_insert(arr, &zv)
	}
	return unsafe.Pointer(arr)
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
	return phpPackedArrayFromItems(items)
}

// scopecache_head returns the oldest items in scope with seq > afterSeq,
// up to `limit`. Companion to scopecache_tail — same null-vs-empty-array
// rules and same per-element array-construction shape.
//
// Use afterSeq=0 to start from the beginning of the scope. Returned items
// are seq-ascending (oldest first); the next call passes the last returned
// seq as afterSeq to paginate forward.
//
// export_php:function scopecache_head(string $scope, int $after_seq, int $limit): ?array
func scopecache_head(scope *C.zend_string, afterSeq int64, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	items, _, found := gw.Head(zendStringView(scope), uint64(afterSeq), int(limit))
	if !found {
		return nil
	}
	return phpPackedArrayFromItems(items)
}

// scopecache_get_by_seq looks up a single item by scope + seq. Same
// shape as scopecache_get, just addressed by the cache-assigned seq
// instead of the client-supplied id.
//
// export_php:function scopecache_get_by_seq(string $scope, int $seq): ?string
func scopecache_get_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetBySeq(zendStringView(scope), uint64(seq))
	if !found {
		return nil
	}
	return phpStringFromBytes(item.Payload)
}

// scopecache_render_by_id returns the rendered (HTTP-wire-shape)
// bytes for an item — same content as the body of GET /render?... in
// the HTTP API. For pre-rendered JSON-string payloads this is the
// shortcut path that bypasses re-serialisation; for other payload
// shapes it re-emits the canonical JSON.
//
// export_php:function scopecache_render_by_id(string $scope, string $id): ?string
func scopecache_render_by_id(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	bytes, found := gw.RenderByID(zendStringView(scope), zendStringView(id))
	if !found {
		return nil
	}
	return phpStringFromBytes(bytes)
}

// scopecache_render_by_seq returns the rendered bytes for an item
// addressed by scope + seq.
//
// export_php:function scopecache_render_by_seq(string $scope, int $seq): ?string
func scopecache_render_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	bytes, found := gw.RenderBySeq(zendStringView(scope), uint64(seq))
	if !found {
		return nil
	}
	return phpStringFromBytes(bytes)
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

// scopecache_upsert creates an item or replaces the payload of an
// existing one at (scope, id). Returns the assigned seq on create,
// the preserved seq on replace — same int either way. Returns 0 on
// validation failure, capacity error, or missing gateway.
//
// Same map-key copy discipline as scopecache_append.
//
// export_php:function scopecache_upsert(string $scope, string $id, string $payload): int
func scopecache_upsert(scope *C.zend_string, id *C.zend_string, payload *C.zend_string) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	item := scopecache.Item{
		Scope:   zendStringCopy(scope),
		ID:      zendStringCopy(id),
		Payload: zendStringBytes(payload),
	}
	result, _, err := gw.Upsert(item)
	if err != nil {
		return 0
	}
	return int64(result.Seq)
}

// scopecache_update replaces the payload of an existing item at
// (scope, id). Returns 1 if an item was updated, 0 if not found OR
// on validation/error. Sprint 3 will disambiguate via exceptions;
// callers that need to distinguish "no such item" from "validation
// failure" must scopecache_get first.
//
// export_php:function scopecache_update(string $scope, string $id, string $payload): int
func scopecache_update(scope *C.zend_string, id *C.zend_string, payload *C.zend_string) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	item := scopecache.Item{
		Scope:   zendStringCopy(scope),
		ID:      zendStringCopy(id),
		Payload: zendStringBytes(payload),
	}
	n, err := gw.Update(item)
	if err != nil {
		return 0
	}
	return int64(n)
}

// scopecache_counter_add atomically adds `by` to the counter at
// (scope, id) and returns the post-add value. Creates the counter
// if missing (starts at 0 before the add, so first call with by=1
// returns 1). Returns 0 on error — note that 0 is ALSO a valid
// counter value, so this sentinel is ambiguous; Sprint 3 will fix
// with exceptions.
//
// export_php:function scopecache_counter_add(string $scope, string $id, int $by): int
func scopecache_counter_add(scope *C.zend_string, id *C.zend_string, by int64) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	value, _, err := gw.CounterAdd(zendStringCopy(scope), zendStringCopy(id), by)
	if err != nil {
		return 0
	}
	return value
}

// scopecache_delete removes the item at (scope, id). Returns 1 if
// an item was deleted, 0 if not found or on error.
//
// Scope/id are NOT retained by scopecache on this path — the delete
// only takes the shard write-lock briefly and removes by-id from
// the index maps. Aliases are safe.
//
// export_php:function scopecache_delete(string $scope, string $id): int
func scopecache_delete(scope *C.zend_string, id *C.zend_string) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	n, err := gw.Delete(zendStringView(scope), zendStringView(id), 0)
	if err != nil {
		return 0
	}
	return int64(n)
}

// scopecache_delete_by_seq removes the item at (scope, seq). Returns
// 1 if deleted, 0 if not found or on error. Companion to
// scopecache_delete for the seq-addressed path.
//
// export_php:function scopecache_delete_by_seq(string $scope, int $seq): int
func scopecache_delete_by_seq(scope *C.zend_string, seq int64) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	n, err := gw.Delete(zendStringView(scope), "", uint64(seq))
	if err != nil {
		return 0
	}
	return int64(n)
}

// scopecache_delete_up_to removes every item in scope with seq <=
// maxSeq. The drain-stream primitive: subscribers tail items, then
// delete_up_to the last seq they processed to free capacity. Returns
// the deleted count, 0 on error.
//
// export_php:function scopecache_delete_up_to(string $scope, int $max_seq): int
func scopecache_delete_up_to(scope *C.zend_string, maxSeq int64) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	n, err := gw.DeleteUpTo(zendStringView(scope), uint64(maxSeq))
	if err != nil {
		return 0
	}
	return int64(n)
}

// scopecache_delete_scope removes the entire scope and every item
// in it. Returns the count of items deleted (which is also a rough
// indicator of whether the scope existed: 0 = not found OR empty
// scope). Reserved scopes (_events, _inbox) are rejected by
// Gateway.DeleteScope and return 0 here.
//
// export_php:function scopecache_delete_scope(string $scope): int
func scopecache_delete_scope(scope *C.zend_string) int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	n, _, err := gw.DeleteScope(zendStringView(scope))
	if err != nil {
		return 0
	}
	return int64(n)
}

// scopecache_wipe drops every scope (user-managed AND reserved),
// resets the byte counter, then re-creates the reserved scopes
// (_events, _inbox) under the same all-shard write lock so
// subscribers do not observe a gap. Returns the pre-wipe
// scope_count (including the reserved scopes that were dropped
// and immediately re-created — a freshly-booted store wiped
// immediately returns 2, not 0).
//
// Use with care: this is the equivalent of /wipe in the HTTP API,
// not a per-scope clear.
//
// export_php:function scopecache_wipe(): int
func scopecache_wipe() int64 {
	gw := defaultSlot.Load()
	if gw == nil {
		return 0
	}
	scopeCount, _, _ := gw.Wipe()
	return int64(scopeCount)
}

// scopecache_stats returns the store-wide snapshot as a JSON-encoded
// string — identical shape to what GET /stats returns over HTTP, just
// without the outer envelope. Callers typically `json_decode(..., true)`
// to get a PHP associative array.
//
// JSON return (rather than a native PHP assoc-array) chosen for
// Sprint 2 to avoid building a from-scratch assoc-array cgo helper
// just for two functions (this one + scopecache_scopelist). The
// PHP-side json_decode cost is ~1-3 µs for a typical stats payload —
// fine for an observability call that fires once per request at most.
//
// Returns "" (empty string, which PHP sees as null via the wrapper-
// patch on RETURN_EMPTY_STRING) when no Caddy module is loaded or
// JSON marshalling fails (latter should be impossible given the
// known struct shape).
//
// export_php:function scopecache_stats(): ?string
func scopecache_stats() unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	stats := gw.Stats()
	bytes, err := json.Marshal(stats)
	if err != nil {
		return nil
	}
	return phpStringFromBytes(bytes)
}

// scopecache_scopelist returns the per-scope detail rows as a
// JSON-encoded array string. Same JSON-vs-native-array trade-off
// as scopecache_stats — caller does json_decode to consume.
//
// Params:
//
//	$prefix — optional filter; pass "" for no filter.
//	$after  — pagination cursor (scope name); pass "" to start
//	          from the beginning.
//	$limit  — page size.
//
// Returns "" / null on no Caddy module or JSON marshal failure.
//
// export_php:function scopecache_scopelist(string $prefix, string $after, int $limit): ?string
func scopecache_scopelist(prefix *C.zend_string, after *C.zend_string, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	entries, _ := gw.ScopeList(zendStringView(prefix), zendStringView(after), int(limit))
	bytes, err := json.Marshal(entries)
	if err != nil {
		return nil
	}
	return phpStringFromBytes(bytes)
}
