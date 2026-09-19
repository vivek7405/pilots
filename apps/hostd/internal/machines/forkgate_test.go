package machines

import (
	"context"
	"errors"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// gateErrStore fails exactly the read that says who may reach a machine's URL.
// Everything else is the real store, so a fork gets as far as it would have.
type gateErrStore struct {
	state.Store
	err error
}

func (s *gateErrStore) GetURLAuth(ctx context.Context, id string) (*state.URLAuth, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.GetURLAuth(ctx, id)
}

// A parent gate that cannot be READ does not produce a public fork.
//
// The check was `if u, err := GetURLAuth(parent); err == nil && ...`, so a
// store that could not answer fell straight through and the fork was created
// with no url_auth row -- and no row reads as PUBLIC. The one moment nothing
// knew whether the parent was gated was the moment the parent's memory and
// disk were published at a new URL, silently, looking exactly like a machine
// whose owner had chosen public.
//
// Restoring `err == nil` makes this return a machine and no error.
func TestAForkIsRefusedWhenTheParentsGateCannotBeRead(t *testing.T) {
	m := forkGateManager(t)
	base := m.opts.Store
	m.opts.Store = &gateErrStore{Store: base, err: errStoreUnwell}

	_, err := m.parentGate(context.Background(), "m-parent")
	if !errors.Is(err, errStoreUnwell) {
		t.Fatalf("parentGate returned %v, want the store error rather than a mode", err)
	}
}

// A parent with NO gate row is a real answer, not a failure: it means public,
// and a fork of it is public too. The distinction between "no row" and "could
// not ask" is the whole of the fix, so it is asserted in both directions.
func TestAParentWithNoGateRowReadsAsPublic(t *testing.T) {
	m := forkGateManager(t)

	mode, err := m.parentGate(context.Background(), "m-never-gated")
	if err != nil {
		t.Fatalf("a parent with no gate row errored: %v", err)
	}
	if mode != "" {
		t.Errorf("mode = %q, want empty: no row means public", mode)
	}
}

// The parent's gate is carried when it can be read.
func TestAParentsGateIsReadBack(t *testing.T) {
	m := forkGateManager(t)
	ctx := context.Background()
	if err := m.opts.Store.PutURLAuth(ctx, &state.URLAuth{
		ID: "m-parent", Kind: "machine", Mode: "org", UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	mode, err := m.parentGate(ctx, "m-parent")
	if err != nil {
		t.Fatalf("parentGate: %v", err)
	}
	if mode != "org" {
		t.Errorf("mode = %q, want org", mode)
	}
}

func forkGateManager(t *testing.T) *Manager {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Manager{opts: Options{HostID: "host-a", Store: st}}
}
