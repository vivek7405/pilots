package quota

import (
	"context"
	"errors"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func openStore(t *testing.T) state.Store {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedMachine writes a machine and the tenancy row naming its owner.
func seedMachine(t *testing.T, st state.Store, id, org, machineState string, vcpus, memMiB int) {
	t.Helper()
	ctx := context.Background()
	if err := st.PutMachine(ctx, &state.Machine{
		ID: id, Name: id, HostID: "host-a", State: machineState,
		VCPUs: vcpus, MemMiB: memMiB,
	}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: id, OrgID: org, Kind: "machine"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
}

func TestDefaultsApplyWithNoRow(t *testing.T) {
	st := openStore(t)
	got := For(context.Background(), st, "org_1")
	if got.MaxMachines != Defaults.MaxMachines || got.OrgID != "org_1" {
		t.Errorf("For with no row = %+v, want the defaults for org_1", got)
	}
	// A missing row is not zero. Zero would freeze every new org the instant
	// it appeared.
	if got.MaxMachines == 0 {
		t.Error("a missing row was read as a limit of zero")
	}
}

func TestCheckRefusesAtTheLimit(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 2, MaxVCPUs: 8, MaxMemMiB: 4096,
		MaxVolumeGiB: 10, MaxBuilds: 1,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}
	seedMachine(t, st, "m_1", "org_1", "running", 1, 512)
	seedMachine(t, st, "m_2", "org_1", "running", 1, 512)

	err := Check(ctx, st, "org_1", Delta{Machines: 1, VCPUs: 1, MemMiB: 512})
	var ex *Exceeded
	if !errors.As(err, &ex) {
		t.Fatalf("Check at the limit returned %v, want *Exceeded", err)
	}
	if ex.Quota != "machines" || ex.Limit != 2 || ex.Used != 2 {
		t.Errorf("Exceeded = %+v, want machines limit 2 used 2", ex)
	}
}

// A destroyed machine holds nothing. Counting tombstones would make an org's
// limit fall over its lifetime rather than over what it is running.
func TestDestroyedAndForeignRowsAreNotCounted(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 2, MaxVCPUs: 8, MaxMemMiB: 4096, MaxVolumeGiB: 10,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}
	seedMachine(t, st, "m_1", "org_1", "running", 1, 512)
	seedMachine(t, st, "m_dead", "org_1", state.StateDestroyed, 1, 512)
	seedMachine(t, st, "m_other", "org_2", "running", 1, 512)
	// A row with no tenancy belongs to nobody and is counted against nobody.
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_legacy", HostID: "host-a", State: "running", VCPUs: 1, MemMiB: 512,
	}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	if err := Check(ctx, st, "org_1", Delta{Machines: 1, VCPUs: 1, MemMiB: 512}); err != nil {
		t.Errorf("Check refused at one live machine of two allowed: %v", err)
	}
}

func TestVCPUAndMemoryAndVolumeLimits(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 100, MaxVCPUs: 4, MaxMemMiB: 1024, MaxVolumeGiB: 10,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}
	seedMachine(t, st, "m_1", "org_1", "running", 4, 512)

	if err := Check(ctx, st, "org_1", Delta{Machines: 1, VCPUs: 1}); err == nil {
		t.Error("a create past the vcpu limit was admitted")
	}
	if err := Check(ctx, st, "org_1", Delta{Machines: 1, MemMiB: 1024}); err == nil {
		t.Error("a create past the memory limit was admitted")
	}

	var ex *Exceeded
	if err := Check(ctx, st, "org_1", Delta{VolumeGiB: 11}); !errors.As(err, &ex) {
		t.Fatalf("a volume past the limit returned %v", err)
	} else if ex.Quota != "volume_gib" {
		t.Errorf("Exceeded names %q, want volume_gib", ex.Quota)
	}
}

// A build is not a replicated object, so the honest limit is per host and the
// refusal says so.
func TestHostGateBoundsConcurrentBuilds(t *testing.T) {
	var g HostGate

	if _, ok := g.Acquire("org_1", 2); !ok {
		t.Fatal("the first build was refused")
	}
	if _, ok := g.Acquire("org_1", 2); !ok {
		t.Fatal("the second build was refused")
	}
	used, ok := g.Acquire("org_1", 2)
	if ok {
		t.Error("a third concurrent build was admitted past a limit of 2")
	}
	if used != 2 {
		t.Errorf("the refusal reported %d in use, want 2", used)
	}
	// Another org is unaffected: the gate is per org, not per host in total.
	if _, ok := g.Acquire("org_2", 2); !ok {
		t.Error("another org was refused by org_1's builds")
	}

	g.Release("org_1")
	if _, ok := g.Acquire("org_1", 2); !ok {
		t.Error("a slot was not returned by Release")
	}
	// Release on an org holding none must not go negative.
	g.Release("org_never")
	g.Release("org_never")
	if _, ok := g.Acquire("org_never", 1); !ok {
		t.Error("an over-released org cannot acquire")
	}
}

// A nil gate is unlimited, so a Deps built without one -- a test, a host with
// no builder -- never has to special-case it.
func TestANilGateIsUnlimited(t *testing.T) {
	var g *HostGate
	for i := 0; i < 5; i++ {
		if _, ok := g.Acquire("org_1", 1); !ok {
			t.Fatal("a nil gate refused")
		}
	}
	g.Release("org_1")
}
