// gateway_registry_test.go — coverage for the process-wide
// *Gateway registry. Pins the contract used by the caddymodule +
// future in-process consumers: register/lookup, nil-deregister, the
// reload-race compare-and-delete, basic concurrent safety, and the
// cached-slot pattern that hot-path consumers (PHP extension, future
// addons) rely on for lock-free per-call reads.

package scopecache

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRegistry_RegisterAndLookup(t *testing.T) {
	gw := NewGateway(Config{})
	RegisterGateway("test-roundtrip", gw)
	t.Cleanup(func() { RegisterGateway("test-roundtrip", nil) })

	if got := LookupGateway("test-roundtrip"); got != gw {
		t.Fatalf("Lookup returned %v, want %v", got, gw)
	}
}

func TestRegistry_LookupMissingReturnsNil(t *testing.T) {
	if got := LookupGateway("does-not-exist-anywhere"); got != nil {
		t.Fatalf("Lookup of missing name returned %v, want nil", got)
	}
}

func TestRegistry_RegisterNilDeregisters(t *testing.T) {
	gw := NewGateway(Config{})
	RegisterGateway("test-nil-dereg", gw)
	RegisterGateway("test-nil-dereg", nil)

	if got := LookupGateway("test-nil-dereg"); got != nil {
		t.Fatalf("after nil-register Lookup returned %v, want nil", got)
	}
}

// Pins the contract that the caddymodule Cleanup path relies on:
// if a newer Provision has already overwritten the registration,
// the older instance's Cleanup must NOT clobber it.
func TestRegistry_DeregisterIfPreservesNewerRegistration(t *testing.T) {
	gwA := NewGateway(Config{})
	gwB := NewGateway(Config{})

	RegisterGateway("test-reload-race", gwA)
	RegisterGateway("test-reload-race", gwB) // simulates new Provision
	t.Cleanup(func() { RegisterGateway("test-reload-race", nil) })

	DeregisterGatewayIf("test-reload-race", gwA) // simulates old Cleanup

	if got := LookupGateway("test-reload-race"); got != gwB {
		t.Fatalf("DeregisterGatewayIf clobbered newer registration: got %v, want %v", got, gwB)
	}
}

// Sanity check that the RWMutex actually protects concurrent access.
// With -race this fires on data races; without it, it at least
// exercises the lock paths from many goroutines simultaneously.
func TestRegistry_ConcurrentSafe(t *testing.T) {
	gw := NewGateway(Config{})
	t.Cleanup(func() { RegisterGateway("test-conc", nil) })

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RegisterGateway("test-conc", gw)
			_ = LookupGateway("test-conc")
			DeregisterGatewayIf("test-conc", gw)
		}()
	}
	wg.Wait()
}

// The slot returned by LookupGatewaySlot must:
//   - be non-nil even before any Register fires (lazy-create).
//   - reflect the current registration on Load (initially nil,
//     becomes gw after Register, becomes nil after the CAS deregister).
//   - be the SAME pointer across calls (stable for the process life).
//
// This is the contract hot-path consumers (PHP extension) build on:
// cache the slot once at init, Load per use, no lookup overhead.
func TestRegistry_LookupGatewaySlot_StableAcrossRegistrations(t *testing.T) {
	gwA := NewGateway(Config{})
	gwB := NewGateway(Config{})
	t.Cleanup(func() { RegisterGateway("test-slot-stable", nil) })

	slot := LookupGatewaySlot("test-slot-stable")
	if slot == nil {
		t.Fatal("LookupGatewaySlot returned nil before any Register; expected lazy-creation")
	}
	if got := slot.Load(); got != nil {
		t.Fatalf("fresh slot.Load = %v, want nil", got)
	}

	RegisterGateway("test-slot-stable", gwA)
	if got := slot.Load(); got != gwA {
		t.Fatalf("after Register(gwA), slot.Load = %v, want gwA", got)
	}

	// Subsequent LookupGatewaySlot must return the SAME slot pointer —
	// otherwise the cached-slot pattern silently breaks.
	if again := LookupGatewaySlot("test-slot-stable"); again != slot {
		t.Fatalf("LookupGatewaySlot returned a different slot pointer on second call: %p vs %p", again, slot)
	}

	// Provision-after-reload pattern: new gateway overwrites the slot,
	// the cached slot pointer transparently sees the new value.
	RegisterGateway("test-slot-stable", gwB)
	if got := slot.Load(); got != gwB {
		t.Fatalf("after Register(gwB), cached slot.Load = %v, want gwB", got)
	}

	// Stale Cleanup must not clobber the new registration.
	DeregisterGatewayIf("test-slot-stable", gwA)
	if got := slot.Load(); got != gwB {
		t.Fatalf("DeregisterGatewayIf(gwA) clobbered gwB through the slot: got %v", got)
	}

	// Real Cleanup clears it.
	DeregisterGatewayIf("test-slot-stable", gwB)
	if got := slot.Load(); got != nil {
		t.Fatalf("after DeregisterGatewayIf(gwB) the slot should be nil, got %v", got)
	}
}

// Concurrent readers cache a slot pointer; concurrent writers swap
// the gateway. Each reader sees a sequence of valid pointers (nil or
// one of the registered gateways) — no torn reads, no UAF.
func TestRegistry_LookupGatewaySlot_ConcurrentLoad(t *testing.T) {
	const name = "test-slot-conc"
	gws := []*Gateway{NewGateway(Config{}), NewGateway(Config{}), NewGateway(Config{})}
	valid := map[*Gateway]bool{nil: true}
	for _, gw := range gws {
		valid[gw] = true
	}
	t.Cleanup(func() { RegisterGateway(name, nil) })

	slot := LookupGatewaySlot(name)

	var wg sync.WaitGroup
	var seenInvalid atomic.Bool
	stop := make(chan struct{})

	// Writers: spin through gws, registering each.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				RegisterGateway(name, gws[i%len(gws)])
			}
		}()
	}

	// Readers: Load via cached slot; assert each load is a valid value.
	for r := 0; r < 16; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50000; i++ {
				got := slot.Load()
				if !valid[got] {
					seenInvalid.Store(true)
					return
				}
			}
		}()
	}

	// Let the readers do their work, then signal writers to stop.
	// Cheap heuristic: writers don't need to outlive readers; once
	// we stop them and Wait, we know we've covered the concurrent
	// window.
	go func() {
		// Yield enough times for all readers to make progress before
		// closing stop. The reader loop is bounded so the test always
		// terminates.
		for i := 0; i < 1000; i++ {
			if seenInvalid.Load() {
				break
			}
		}
		close(stop)
	}()

	wg.Wait()

	if seenInvalid.Load() {
		t.Fatal("concurrent Load via cached slot observed an invalid *Gateway pointer (torn read)")
	}
}
