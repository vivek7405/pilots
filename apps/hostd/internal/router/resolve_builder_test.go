package router

import (
	"context"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A builder is refused on the path that actually runs: the subscription cache.
//
// TestResolveRefusesABuilderMachine asserts the same property, with no Lookup
// configured -- so it exercised only the store fallback, and the guard lived
// only there. The cache is the steady state; a miss is a row the subscription
// has not delivered yet. So in ordinary running a builder's address resolved,
// woke a quota-exempt 4 vCPU machine nobody asked for, and reset the activity
// clock the stale-builder collector reads. An org can read its own builder's
// name out of the machine list, so that was reachable by anyone with a key.
//
// Dropping the !machines.IsBuilder check from the Lookup branch reds this.
func TestTheCacheRefusesABuilderToo(t *testing.T) {
	const builder = "builder-acme0a1b2c-host0d1e2f"
	row := state.Machine{
		ID: "m-b", Name: builder, HostID: "host-a",
		Domain: builder + ".pilotrun.app", State: "running",
	}
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		// The store does not know it at all, so anything that resolves came
		// from the cache.
		Store: &stubStore{},
		Lookup: func(name string) (state.Machine, bool) {
			if name == builder {
				return row, true
			}
			return state.Machine{}, false
		},
	})

	if target, err := r.resolve(context.Background(), builder+".pilotrun.app"); err == nil {
		t.Errorf("a builder resolved from the cache to %q", target.Machine.ID)
	}
	if target, err := r.resolve(context.Background(), builder+".otherdomain.test"); err == nil {
		t.Errorf("a builder resolved from the cache by name to %q", target.Machine.ID)
	}
}

// The same row under an ordinary name still resolves from the cache, so the
// refusal above is the builder prefix and not the cache path being broken.
func TestTheCacheStillResolvesAnOrdinaryMachine(t *testing.T) {
	row := state.Machine{
		ID: "m-1", Name: "alpha", HostID: "host-a",
		Domain: "alpha.pilotrun.app", State: "running",
	}
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{},
		Lookup: func(name string) (state.Machine, bool) {
			if name == "alpha" {
				return row, true
			}
			return state.Machine{}, false
		},
	})

	target, err := r.resolve(context.Background(), "alpha.pilotrun.app")
	if err != nil {
		t.Fatalf("an ordinary machine did not resolve from the cache: %v", err)
	}
	if target.Machine.ID != "m-1" {
		t.Errorf("resolved to %q, want m-1", target.Machine.ID)
	}
}
