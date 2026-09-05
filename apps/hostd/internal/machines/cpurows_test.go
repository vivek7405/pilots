package machines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The cpu rows a machine leaves behind when it is destroyed.
//
// machine_cpu is gossiped to the whole fleet and materialized into a map in
// every host's cache, so one row per destroyed machine and one per deleted
// checkpoint is a leak replicated as many times as there are hosts -- worse
// than a local one, and invisible until the table is large. Destroy is the
// only place a machine's row and its checkpoints' rows can be removed, because
// it is the only place that knows they are going.
func TestDestroyRemovesTheMachineAndCheckpointCPURows(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()
	st := m.opts.Store

	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m-1", Name: "m-1", HostID: "host-a", State: "stopped",
		VCPUs: 1, MemMiB: 512,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck-1", MachineID: "m-1", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	// A second machine with its own rows: the counterfactual for a destroy
	// that took more than its own.
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m-2", Name: "m-2", HostID: "host-a", State: "stopped",
		VCPUs: 1, MemMiB: 512,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck-2", MachineID: "m-2", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, kind string }{
		{"m-1", state.KindMachine}, {"ck-1", state.KindCheckpoint},
		{"m-2", state.KindMachine}, {"ck-2", state.KindCheckpoint},
	} {
		if err := st.PutMachineCPU(ctx, &state.MachineCPU{
			ID: r.id, Kind: r.kind, Vendor: "AuthenticAMD"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.Destroy(ctx, "m-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	for _, id := range []string{"m-1", "ck-1"} {
		if _, err := st.GetMachineCPU(ctx, id); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("the cpu row for %s outlived the destroy: %v", id, err)
		}
	}
	for _, id := range []string{"m-2", "ck-2"} {
		if _, err := st.GetMachineCPU(ctx, id); err != nil {
			t.Errorf("destroying m-1 took %s's cpu row with it: %v", id, err)
		}
	}
}

// cpuErrStore makes GetMachineCPU fail the way a corrosion query fails: not
// with ErrNotFound, and not because the row is absent.
type cpuErrStore struct {
	state.Store
	err error
}

func (s *cpuErrStore) GetMachineCPU(ctx context.Context, id string) (*state.MachineCPU, error) {
	return nil, s.err
}

// errStoreUnwell is a transient store failure, not a missing row.
var errStoreUnwell = errors.New("corrosion: query timed out")

// A store that cannot answer "which pool photographed this" refuses the
// bring-up rather than picking a path.
//
// Falling through would take one of two bad guesses. Restoring a foreign image
// fails later inside Firecracker as "corrupt snapshot", with nothing in the
// log saying the vendor lookup is why. Cold-booting is worse: recordStart
// clears the memory build and deletes the suspend image from object storage,
// so a cold boot taken on a store hiccup destroys an image that would have
// restored. A refused bring-up is retried by the next request.
func TestABringUpRefusesWhenTheCPUPoolCannotBeRead(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()
	base := m.opts.Store

	row := &state.Machine{
		ID: "m-1", VCPUs: 1, MemMiB: 512,
		MemBuildID: uuid.NewString(), TemplateRootfsBuildID: uuid.NewString(),
	}

	m.opts.Store = &cpuErrStore{Store: base, err: errStoreUnwell}
	_, kind, err := m.bringUp(ctx, row)
	if !errors.Is(err, errStoreUnwell) {
		t.Fatalf("bringUp with an unreadable cpu row returned %v, want the store error", err)
	}
	if !strings.Contains(err.Error(), "CPU pool") {
		t.Errorf("the error does not say the vendor lookup is why: %v", err)
	}
	if kind != "" {
		t.Errorf("a refused bring-up reported a start kind of %q", kind)
	}

	// The counterfactual: ErrNotFound is NOT a failure. It means no row, which
	// is what every image taken before this table looks like, and it must
	// still fall through to the restore path.
	m.opts.Store = &cpuErrStore{Store: base, err: state.ErrNotFound}
	_, kind, err = m.bringUp(ctx, row)
	if err == nil {
		t.Fatal("the restore succeeded without Firecracker; the test proves nothing")
	}
	if errors.Is(err, errStoreUnwell) || strings.Contains(err.Error(), "CPU pool") {
		t.Errorf("an absent cpu row was treated as a store failure: %v", err)
	}
	if kind != state.StartRestore {
		t.Errorf("an absent cpu row took the %q path, want %q", kind, state.StartRestore)
	}
}

// The same rule on the rollback path, which asks the question about the
// CHECKPOINT's id rather than the machine's.
func TestARollbackRefusesWhenTheCPUPoolCannotBeRead(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()
	base := m.opts.Store

	row := &state.Machine{
		ID: "m-1", VCPUs: 1, MemMiB: 512, TemplateRootfsBuildID: uuid.NewString(),
	}
	ckpt := &state.Checkpoint{ID: "ck-1", MachineID: "m-1", Seq: 1, MemBuildID: uuid.NewString()}

	// awaitRestorable polls the checkpoint's local directory; the marker is
	// what says the chunks are usable, so writing it keeps this test off the
	// five-minute timeout.
	dir := m.checkpointDir(row.ID, ckpt.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".durable"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m.opts.Store = &cpuErrStore{Store: base, err: errStoreUnwell}
	_, kind, err := m.restoreFromCheckpoint(ctx, row, ckpt)
	if !errors.Is(err, errStoreUnwell) {
		t.Fatalf("restoreFromCheckpoint with an unreadable cpu row returned %v, want the store error", err)
	}
	if kind != "" {
		t.Errorf("a refused rollback reported a start kind of %q", kind)
	}

	// The counterfactual: an absent row still rolls back.
	m.opts.Store = &cpuErrStore{Store: base, err: state.ErrNotFound}
	_, _, err = m.restoreFromCheckpoint(ctx, row, ckpt)
	if err == nil {
		t.Fatal("the rollback succeeded without Firecracker; the test proves nothing")
	}
	if errors.Is(err, errStoreUnwell) || strings.Contains(err.Error(), "CPU pool") {
		t.Errorf("an absent cpu row was treated as a store failure: %v", err)
	}
}
