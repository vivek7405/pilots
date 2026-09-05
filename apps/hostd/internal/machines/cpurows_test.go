package machines

import (
	"context"
	"errors"
	"testing"

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
