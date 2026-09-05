package machines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// redeployManager is a Manager with just enough on disk to reach the boot.
//
// The boot itself cannot run here -- there is no Firecracker, no object store
// and no template -- and that is fine: everything this file asserts happens
// BEFORE the boot, and a Redeploy that gets as far as failing there has done
// all of it.
func redeployManager(t *testing.T) (*Manager, state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return &Manager{opts: Options{
		HostID:    "host-a",
		Store:     store,
		StateRoot: t.TempDir(),
		CacheRoot: t.TempDir(),
	}}, store
}

// A SUSPENDED replica's copy-on-write file is discarded too.
//
// Redeploy's kill branch runs only when the registry holds a live process,
// and a volume-backed service takes the ordinary floor of zero -- so the
// deploy after the first one arrives with the replica suspended, took the
// other branch, and left the cow file describing the OLD image sitting in the
// state dir the new boot is about to reuse. One leaked file per deploy.
func TestRedeployDiscardsTheCowOfASuspendedMachine(t *testing.T) {
	ctx := context.Background()
	m, store := redeployManager(t)

	row := &state.Machine{ID: "m_1", Name: "db", HostID: "host-a",
		State: StateSuspended, ServiceID: "svc_1", ReleaseID: "rel_1", Slot: 7}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	cow := fc.CowPath(m.stateDir(row.ID))
	if err := os.MkdirAll(filepath.Dir(cow), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cow, []byte("every write since the last snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Redeploy(ctx, row.ID, api.RedeployRequest{
		Image: "bld_new", Release: "rel_2",
	}); err == nil {
		t.Fatal("the boot was expected to fail in a test process")
	}

	if _, err := os.Stat(cow); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the suspended replica's cow file survived the redeploy (stat: %v); "+
			"the new boot reuses that state dir", err)
	}
}
