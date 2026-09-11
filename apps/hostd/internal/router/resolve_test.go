package router

import (
	"context"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// stubStore serves a fixed set of rows from "local" state.
type stubStore struct {
	state.Store
	machines []state.Machine
	services []state.Service
	// servicesPanic proves the subscription cache was consulted instead of
	// the store, the way crosshost_test.go does it for Lookup.
	servicesPanic bool
}

func (s *stubStore) ListMachines(context.Context) ([]state.Machine, error) {
	return s.machines, nil
}

func (s *stubStore) ListServices(context.Context) ([]state.Service, error) {
	if s.servicesPanic {
		panic("the store was read for a service the cache already answered")
	}
	return s.services, nil
}

// TouchMachine is a no-op: serveLocally records activity in a goroutine, and
// the embedded nil state.Store would panic there.
func (s *stubStore) TouchMachine(context.Context, string, int64) error { return nil }

func (s *stubStore) GetMachine(_ context.Context, id string) (*state.Machine, error) {
	for i := range s.machines {
		if s.machines[i].ID == id {
			m := s.machines[i]
			return &m, nil
		}
	}
	return nil, state.ErrNotFound
}

func TestResolveFindsLocalMachine(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-1", Name: "webapp", HostID: "host-a", Domain: "webapp.pilotrun.app"},
		}},
	})

	target, err := r.resolve(context.Background(), "webapp.pilotrun.app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Machine.ID != "m-1" {
		t.Errorf("resolved to %q", target.Machine.ID)
	}
}

// A machine owned by another host must not be woken here.
//
// Doing so would run a second copy from the same artifacts and write state onto
// a row this host does not own -- a single-writer violation that, once the store
// is replicated, corrupts silently through the merge rather than erroring.
// Any host resolves any machine. DNS points every workload name at every
// host, so a request for someone else's machine is ordinary -- what differs is
// only where it is then served, which serveOrForward decides.
func TestResolveFindsAMachineOwnedElsewhere(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-2", Name: "elsewhere", HostID: "host-b", Domain: "elsewhere.pilotrun.app"},
		}},
	})

	target, err := r.resolve(context.Background(), "elsewhere.pilotrun.app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Machine.HostID != "host-b" {
		t.Errorf("owner = %q, want host-b so the caller can forward there",
			target.Machine.HostID)
	}
}

// With no HostID configured (a single-host deployment that never set one) the
// filter must not lock the operator out of their own machines.
func TestResolveWithoutHostIDServesEverything(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-3", Name: "webapp", HostID: "host-b", Domain: "webapp.pilotrun.app"},
		}},
	})
	if _, err := r.resolve(context.Background(), "webapp.pilotrun.app"); err != nil {
		t.Errorf("resolve: %v", err)
	}
}

func TestResolveUnknownName(t *testing.T) {
	r := New(Options{Domain: "pilotrun.app", HostID: "host-a", Store: &stubStore{}})
	if _, err := r.resolve(context.Background(), "nope.pilotrun.app"); err == nil {
		t.Error("resolved a name that does not exist")
	}
}

// A builder machine is not routable, by name or by domain.
//
// It serves no application, so a request to its address can only fail. What it
// would do FIRST is wake it, and an org can read its own builder's name out of
// the machine list, so without this a tenant could hold a quota-exempt 4 vCPU
// machine awake indefinitely by curling a URL, with every hit resetting the
// activity clock the stale-builder collector reads.
func TestResolveRefusesABuilderMachine(t *testing.T) {
	const builder = "builder-acme0a1b2c-host0d1e2f"
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-b", Name: builder, HostID: "host-a",
				Domain: builder + ".pilotrun.app"},
		}},
	})

	// By the domain the create stamped on the row.
	if target, err := r.resolve(context.Background(), builder+".pilotrun.app"); err == nil {
		t.Errorf("a builder resolved by domain to %q", target.Machine.ID)
	}
	// And by name, which is the half that clearing the domain would miss:
	// the lookup matches either.
	if target, err := r.resolve(context.Background(), builder+".otherdomain.test"); err == nil {
		t.Errorf("a builder resolved by name to %q", target.Machine.ID)
	}

	// Counterfactual: the same row under an ordinary name resolves, so the
	// refusal is the builder prefix and not something else about the row.
	r2 := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-b", Name: "ordinary", HostID: "host-a",
				Domain: "ordinary.pilotrun.app"},
		}},
	})
	if _, err := r2.resolve(context.Background(), "ordinary.pilotrun.app"); err != nil {
		t.Fatalf("an ordinary machine stopped resolving: %v", err)
	}
}
