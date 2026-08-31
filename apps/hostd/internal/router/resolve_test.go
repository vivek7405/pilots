package router

import (
	"context"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// stubStore serves a fixed set of rows from "local" state.
type stubStore struct {
	state.Store
	machines []state.Machine
}

func (s *stubStore) ListMachines(context.Context) ([]state.Machine, error) {
	return s.machines, nil
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
func TestResolveRefusesAnotherHostsMachine(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-2", Name: "elsewhere", HostID: "host-b", Domain: "elsewhere.pilotrun.app"},
		}},
	})

	_, err := r.resolve(context.Background(), "elsewhere.pilotrun.app")
	if err == nil {
		t.Fatal("resolved a machine owned by another host")
	}
	if !strings.Contains(err.Error(), "host-b") {
		t.Errorf("the error should name the owning host, got: %v", err)
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
