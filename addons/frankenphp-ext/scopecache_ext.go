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
// Expected order-of-magnitude win at large payloads is somewhere
// between 10× and 50× over the current HTTP path. Numbers come later;
// for now this is a single-function prototype that proves the
// build-chain works and the wire-shape is sane.
//
// Boundary discipline (CLAUDE.md):
//   - This is an addon. The core never changes shape to accommodate it.
//   - The shared-Gateway question (Caddy module + PHP extension hitting
//     the same in-process cache) is a follow-up refactor; this prototype
//     uses an addon-owned Gateway initialised lazily on first call.

package scopecache_ext

// #include <Zend/zend_types.h>
import "C"

import (
	"sync"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
	"github.com/dunglas/frankenphp"
)

// ensureGateway returns the process-wide addon Gateway, initialising
// it on first call. The Once-protected init is safe even when PHP
// requests arrive on multiple worker threads simultaneously.
//
// Caps are hard-coded for the prototype. A production version would
// either honour the same env vars the standalone binary reads
// (SCOPECACHE_SCOPE_MAX_ITEMS etc.) or be wired to the Caddy module's
// Gateway via a process-global singleton — that's the "shared
// Gateway" refactor referenced in the file header.
var (
	gwOnce sync.Once
	gw     *scopecache.Gateway
)

func ensureGateway() *scopecache.Gateway {
	gwOnce.Do(func() {
		gw = scopecache.NewGateway(scopecache.Config{
			ScopeMaxItems: 100_000,
			MaxStoreBytes: 100 << 20, // 100 MiB
			MaxItemBytes:  1 << 20,   // 1 MiB
		})

		// Seed a single item so a fresh test.php sees something
		// without first calling an /append-style function the
		// extension does not yet expose. Drop this seed once
		// scopecache_append lands.
		_, _ = gw.Append(scopecache.Item{
			Scope:   "demo",
			ID:      "hello",
			Payload: []byte(`{"greeting":"hi from scopecache, via cgo, in-process"}`),
		})
	})
	return gw
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
// export_php:function scopecache_get(string $scope, string $id): ?string
func scopecache_get(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	scopeStr := frankenphp.GoString(unsafe.Pointer(scope))
	idStr := frankenphp.GoString(unsafe.Pointer(id))

	item, found := ensureGateway().GetByID(scopeStr, idStr)
	if !found {
		return nil
	}
	return frankenphp.PHPString(string(item.Payload), false)
}
