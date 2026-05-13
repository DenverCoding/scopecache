// gateway_registry.go — process-wide named *Gateway lookup for
// in-process consumers that don't otherwise have a handle on the
// gateway the Caddy module is serving from.
//
// Why this exists: when scopecache runs as a Caddy module, the
// adapter's Provision() creates a *Gateway and wires it into
// *API + the HTTP mux. PHP extensions, future Go-side addons, or
// any other in-process caller that wants to hit the *same* cache
// instance the HTTP routes serve cannot reach the Caddy adapter's
// gateway through ordinary import — the field is unexported and
// the adapter does not expose an accessor.
//
// The registry solves that without coupling the core to any
// specific adapter. The caddymodule registers its gateway under a
// conventional name during Provision() and deregisters during
// Cleanup(); consumers (typically a single hard-coded "default")
// look it up at use time.
//
// Storage shape: a name maps to an atomic.Pointer slot, not directly
// to a *Gateway. The slot is created lazily on first reference and
// is stable for the process lifetime — only its contents swap when
// RegisterGateway / DeregisterGatewayIf fires. This lets hot-path
// consumers cache the slot once and call .Load() per use (~1-2 ns,
// lock-free), instead of paying the registry's RLock on every call.
// See LookupGatewaySlot below.
//
// Lifecycle on Caddy config reload:
//
//   1. Old instance running, gw_old stored in the "default" slot.
//   2. New Provision() runs first, atomically stores gw_new into the
//      same slot. Any cached slot held by a consumer sees gw_new on
//      the next Load.
//   3. Caddy switches traffic to the new instance.
//   4. Old Cleanup() runs. DeregisterGatewayIf does a CAS: only
//      stores nil if the slot still equals gw_old (it doesn't —
//      step 2 already overwrote it). New registration intact.
//
// Map invariant: slots are never removed from the map. "Deregister"
// = atomic Store(nil) into the slot. This preserves the slot pointer
// any consumer already cached, so they keep observing the right
// answer (currently nil) instead of holding a dangling reference to a
// no-longer-mapped slot. The map can only grow by registered names,
// which in practice is one ("default").
//
// In-flight cgo or Go calls holding a *Gateway pointer obtained from
// a previous Load() stay valid: Go's GC will not free a *Gateway
// anyone still references. Subsequent Load()s pick up the new
// pointer atomically.

package scopecache

import (
	"sync"
	"sync/atomic"
)

var (
	registryMu    sync.RWMutex
	registrySlots = map[string]*atomic.Pointer[Gateway]{}
)

// slotForName returns the slot for name, lazily creating it under
// the write lock when it doesn't yet exist. The returned slot is
// stable for the process lifetime — callers may cache it.
func slotForName(name string) *atomic.Pointer[Gateway] {
	registryMu.RLock()
	slot, ok := registrySlots[name]
	registryMu.RUnlock()
	if ok {
		return slot
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if slot, ok = registrySlots[name]; ok {
		return slot
	}
	slot = &atomic.Pointer[Gateway]{}
	registrySlots[name] = slot
	return slot
}

// RegisterGateway publishes gw under name. Pass a nil gw to clear
// the slot unconditionally — use DeregisterGatewayIf instead from
// Cleanup-style paths that must avoid clobbering a newer registration
// during config reload.
func RegisterGateway(name string, gw *Gateway) {
	slotForName(name).Store(gw)
}

// LookupGateway returns the *Gateway currently registered under name,
// or nil if nothing is registered there. Convenience wrapper around
// LookupGatewaySlot().Load() — for hot-path consumers, cache the
// slot once via LookupGatewaySlot and call .Load() per use instead.
func LookupGateway(name string) *Gateway {
	return slotForName(name).Load()
}

// LookupGatewaySlot returns the atomic slot for name, lazily creating
// it if needed. The slot pointer is stable for the process lifetime —
// callers cache it and call .Load() per use:
//
//	var defaultSlot = scopecache.LookupGatewaySlot("default")
//	gw := defaultSlot.Load()  // ~1-2 ns, lock-free, picks up reloads
//
// If a slot is reachable before any RegisterGateway fires for that
// name (e.g. an init-time lookup that races ahead of Caddy's
// Provision), Load returns nil; the caller must handle that. When
// Provision later Stores into the slot, the cached pointer
// transparently picks up the new value on the next Load.
func LookupGatewaySlot(name string) *atomic.Pointer[Gateway] {
	return slotForName(name)
}

// DeregisterGatewayIf atomically clears the slot only if its current
// pointer equals gw. Safe for caddymodule Cleanup paths: preserves a
// newer Provision's registration during reload, while still clearing
// the slot when the module is fully removed.
func DeregisterGatewayIf(name string, gw *Gateway) {
	slotForName(name).CompareAndSwap(gw, nil)
}
