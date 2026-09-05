package machines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A machine that has checkpoints is refused, not quietly corrupted.
//
// The redeploy repoints the row's template at the new image and clears
// CacheRoot/machines/<id>, which is where every checkpoint's local data
// lives -- while the checkpoint rows and their S3 builds stay. A restore
// afterwards either blocks forever on a directory nothing will repopulate,
// or resolves and applies a diff against a base it was never taken from.
// POST /v1/machines/{id}/redeploy is public, so that was reachable on any
// machine a tenant owns.
func TestRedeployIsRefusedWhileACheckpointExists(t *testing.T) {
	ctx := context.Background()
	m, store := redeployManager(t)

	row := &state.Machine{ID: "m_1", Name: "db", HostID: "host-a", State: StateRunning}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	for _, id := range []string{"ck_1", "ck_2"} {
		if err := store.PutCheckpoint(ctx, &state.Checkpoint{
			ID: id, MachineID: row.ID, RootfsBuildID: "bld_old",
		}); err != nil {
			t.Fatalf("PutCheckpoint: %v", err)
		}
	}

	// A file where the boot would find the state dir, so a redeploy that got
	// past the refusal would be visible as its removal.
	cow := fc.CowPath(m.stateDir(row.ID))
	if err := os.MkdirAll(filepath.Dir(cow), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cow, []byte("."), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := m.Redeploy(ctx, row.ID, api.RedeployRequest{Image: "bld_new", Release: "rel_2"})
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("Redeploy of a machine with checkpoints: %v, want an api.ErrConflict", err)
	}
	for _, want := range []string{"ck_1", "ck_2", "delete them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	// Refused BEFORE anything is torn down: the machine is exactly as it was.
	if _, statErr := os.Stat(cow); statErr != nil {
		t.Errorf("the refused redeploy still touched the state dir: %v", statErr)
	}
	if got, _ := store.GetMachine(ctx, row.ID); got.State != StateRunning {
		t.Errorf("the refused redeploy left the row in %q, want %q", got.State, StateRunning)
	}
}

// And a machine with no checkpoints is not refused: it gets as far as the
// boot, which is as far as anything gets in a test process.
func TestRedeployWithNoCheckpointsIsNotRefused(t *testing.T) {
	ctx := context.Background()
	m, store := redeployManager(t)

	row := &state.Machine{ID: "m_1", Name: "db", HostID: "host-a", State: StateRunning}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	_, err := m.Redeploy(ctx, row.ID, api.RedeployRequest{Image: "bld_new", Release: "rel_2"})
	if err == nil {
		t.Fatal("the boot was expected to fail in a test process")
	}
	if errors.Is(err, api.ErrConflict) {
		t.Errorf("a machine with no checkpoints was refused: %v", err)
	}
}
