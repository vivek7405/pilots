package quota

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// stageCheckpoint writes a durable marker the way a finished upload does.
func stageCheckpoint(t *testing.T, root, machineID, ckID string, bytes int64) {
	t.Helper()
	dir := filepath.Join(root, machineID, "checkpoints", ckID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]int64{"bytes": bytes})
	if err := os.WriteFile(filepath.Join(dir, ".durable"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func quotaStore(t *testing.T) state.Store {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// An org with no snapshot row is every org that existed before the table. It
// must get the default, because zero would refuse its next checkpoint.
func TestAnOrgWithNoSnapshotRowGetsTheDefault(t *testing.T) {
	st := quotaStore(t)
	ctx := context.Background()

	// A quota row with every other limit set and no snapshot row beside it,
	// which is what an upgraded fleet has.
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 5, MaxVCPUs: 10, MaxMemMiB: 2048,
		MaxVolumeGiB: 10, MaxBuilds: 1,
	}); err != nil {
		t.Fatal(err)
	}

	limits, err := limitsFor(ctx, st, "org_1")
	if err != nil {
		t.Fatalf("limitsFor: %v", err)
	}
	if limits.MaxSnapshotGiB != Defaults.MaxSnapshotGiB {
		t.Errorf("max_snapshot_gib = %d, want the default %d; zero would refuse "+
			"every checkpoint on an upgraded fleet",
			limits.MaxSnapshotGiB, Defaults.MaxSnapshotGiB)
	}
	// And the limits it DID set survive the dual read.
	if limits.MaxMachines != 5 {
		t.Errorf("max_machines = %d, want 5", limits.MaxMachines)
	}
}

// The limit round-trips through the store, in its own table.
func TestTheSnapshotLimitRoundTrips(t *testing.T) {
	st := quotaStore(t)
	ctx := context.Background()

	if err := st.PutQuota(ctx, &state.Quota{OrgID: "org_1", MaxSnapshotGiB: 7}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetQuota(ctx, "org_1")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if got.MaxSnapshotGiB != 7 {
		t.Errorf("max_snapshot_gib = %d, want 7", got.MaxSnapshotGiB)
	}
}

// The counter reads what exists rather than a running total, which is what
// makes it self-healing after a failed upload or a retention pass.
func TestSnapshotGiBCountsTheStagedCheckpoints(t *testing.T) {
	root := t.TempDir()
	old := CheckpointRoot
	SetCheckpointRoot(root)
	t.Cleanup(func() { CheckpointRoot = old })

	st := quotaStore(t)
	ctx := context.Background()
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck_1", MachineID: "m_1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck_2", MachineID: "m_1"}); err != nil {
		t.Fatal(err)
	}
	stageCheckpoint(t, root, "m_1", "ck_1", 2<<30)
	stageCheckpoint(t, root, "m_1", "ck_2", 1<<30)

	owned := map[string]struct{}{"m_1": {}}
	if got := snapshotGiBOf(ctx, st, owned); got != 3 {
		t.Errorf("snapshotGiBOf = %d, want 3", got)
	}

	// A checkpoint another host staged is invisible here, which under-counts.
	// That is the right direction for a soft cap: it admits a caller who is
	// over rather than refusing one who is not.
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck_elsewhere", MachineID: "m_1"}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotGiBOf(ctx, st, owned); got != 3 {
		t.Errorf("snapshotGiBOf = %d after a remote checkpoint, want the same 3", got)
	}
}

// The cap refuses when the org is over it, and names the dimension so a client
// is told what to raise.
func TestCheckRefusesAnOrgOverItsSnapshotLimit(t *testing.T) {
	root := t.TempDir()
	old := CheckpointRoot
	SetCheckpointRoot(root)
	t.Cleanup(func() { CheckpointRoot = old })

	st := quotaStore(t)
	ctx := context.Background()
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_1", Kind: "machine", OrgID: "org_1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutQuota(ctx, &state.Quota{OrgID: "org_1", MaxSnapshotGiB: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck_1", MachineID: "m_1"}); err != nil {
		t.Fatal(err)
	}
	stageCheckpoint(t, root, "m_1", "ck_1", 5<<30)

	err := Check(ctx, st, "org_1", Delta{SnapshotGiB: 1})
	if err == nil {
		t.Fatal("a checkpoint was admitted for an org over its snapshot limit")
	}
	var ex *Exceeded
	if !asExceeded(err, &ex) {
		t.Fatalf("err = %v, want an Exceeded", err)
	}
	if ex.Quota != "snapshot_gib" {
		t.Errorf("refused on %q, want snapshot_gib", ex.Quota)
	}
	if ex.Limit != 2 || ex.Used != 5 {
		t.Errorf("Exceeded = %+v, want limit 2 used 5", ex)
	}
}

// A request that is not about snapshots must not pay for counting them.
func TestCheckSkipsTheCountWhenNothingAsksForIt(t *testing.T) {
	root := t.TempDir()
	old := CheckpointRoot
	SetCheckpointRoot(root)
	t.Cleanup(func() { CheckpointRoot = old })

	st := quotaStore(t)
	ctx := context.Background()
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_1", Kind: "machine", OrgID: "org_1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 5, MaxSnapshotGiB: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck_1", MachineID: "m_1"}); err != nil {
		t.Fatal(err)
	}
	stageCheckpoint(t, root, "m_1", "ck_1", 50<<30)

	// A machine create, with the org far over its snapshot limit: admitted,
	// because a snapshot limit is not a machine limit.
	if err := Check(ctx, st, "org_1", Delta{Machines: 1}); err != nil {
		t.Errorf("a machine create was refused by the snapshot limit: %v", err)
	}
}

// asExceeded is errors.As with the package's own type, kept local so the test
// reads as one assertion.
func asExceeded(err error, out **Exceeded) bool {
	if e, ok := err.(*Exceeded); ok {
		*out = e
		return true
	}
	return false
}
