package machines

import (
	"errors"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A machine that had idled to sleep before its host drained is not woken by
// the host that takes it. The row moves; the next request wakes it there.
// bringUp is never reached here: the manager has no slot pool, so reaching it
// would panic.
func TestTakeLeavesAnAsleepMachineAsleep(t *testing.T) {
	m, st := storeManager(t)
	ctx := t.Context()
	row := state.Machine{ID: "m_asleep", Name: "asleep", HostID: "host-b",
		State: StateSuspended, VCPUs: 1, MemMiB: 512}
	if err := st.PutMachine(ctx, &row); err != nil {
		t.Fatal(err)
	}
	offer := &state.Handoff{ID: newID(handoffSuspendedPrefix), MachineID: row.ID,
		FromHost: "host-b", ToHost: "host-a", Seq: 1}
	if err := st.PutHandoff(ctx, offer); err != nil {
		t.Fatal(err)
	}

	if err := m.Take(ctx, row.ID, offer.ID); err != nil {
		t.Fatalf("Take: %v", err)
	}
	got, err := st.GetMachine(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostID != "host-a" || got.State != StateSuspended {
		t.Errorf("after Take: host %s state %s; want host-a, still suspended", got.HostID, got.State)
	}
}

// A machine that was running is brought back, but only onto a host with room:
// the capacity check comes before the claim, so a refusal leaves the offer to
// time out and the source to try the next host.
func TestTakeRefusesARunningMachineWithoutRoomBeforeClaiming(t *testing.T) {
	m, st := storeManager(t)
	m.draining.Store(true) // admit's cheapest refusal
	ctx := t.Context()
	row := state.Machine{ID: "m_busy", Name: "busy", HostID: "host-b",
		State: StateSuspended, VCPUs: 1, MemMiB: 512}
	if err := st.PutMachine(ctx, &row); err != nil {
		t.Fatal(err)
	}
	offer := &state.Handoff{ID: newID(handoffRunningPrefix), MachineID: row.ID,
		FromHost: "host-b", ToHost: "host-a", Seq: 1}
	if err := st.PutHandoff(ctx, offer); err != nil {
		t.Fatal(err)
	}

	err := m.Take(ctx, row.ID, offer.ID)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Take = %v, want ErrNoCapacity", err)
	}
	got, _ := st.GetMachine(ctx, row.ID)
	if got.HostID != "host-b" {
		t.Errorf("the machine was claimed by %s although it could not run there", got.HostID)
	}
}

func TestOffersFromOlderHostsStillResume(t *testing.T) {
	if !takeResumes("ho-0123abcd") {
		t.Error("an offer from a host that predates the suspended marker was left down")
	}
	if !takeResumes(newID(handoffRunningPrefix)) || takeResumes(newID(handoffSuspendedPrefix)) {
		t.Error("the running and suspended markers are read the wrong way round")
	}
}
