package machines

import (
	"errors"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
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

// A checkpoint holds memory and the root disk, not the volume, so a fork of a
// checkpoint of a volume-backed machine would pair that memory with the
// volume as it is now. Refused, rather than a fork that mounts and fails later.
func TestForkingACheckpointOfAVolumeMachineIsRefused(t *testing.T) {
	m, st := storeManager(t)
	ctx := t.Context()
	if err := st.PutMachine(ctx, &state.Machine{ID: "m_db", Name: "db", HostID: "host-a",
		State: StateRunning, VolumeID: "vol_1", VCPUs: 1, MemMiB: 512}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck-1", MachineID: "m_db", Seq: 1,
		MemBuildID: "mem", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Fork(ctx, api.ForkOptions{Checkpoint: "ck-1", Volume: true})
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("Fork = %v, want ErrConflict", err)
	}
}

// Removing a service's last machine removes every row keyed on the service,
// not only its labels and url auth: its size, its broker grant, its releases,
// and each release's vmstate and CPU-pool rows all replicate to every host.
func TestReleasingAServiceLeavesNoServiceRowsBehind(t *testing.T) {
	m, st := storeManager(t)
	ctx := t.Context()
	svc := &state.Service{ID: "svc_gone", Name: "web", ReleaseID: "rel_2", Replicas: 1}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.PutService(ctx, svc))
	for _, id := range []string{"rel_1", "rel_2"} {
		must(st.PutRelease(ctx, &state.Release{ID: id, ServiceID: svc.ID, CreatedAt: 1}))
		must(st.PutReleaseSnapshot(ctx, &state.ReleaseSnapshot{ID: id, ServiceID: svc.ID,
			MachineID: "m_1", CheckpointID: "ck_" + id, CreatedAt: 1}))
		must(st.PutMachineCPU(ctx, &state.MachineCPU{ID: id, Kind: state.KindRelease, Vendor: "AuthenticAMD"}))
	}
	must(st.PutServiceSize(ctx, &state.ServiceSize{ServiceID: svc.ID, VCPUs: 1, MemMiB: 512}))
	must(st.PutBrokerGrant(ctx, &state.BrokerGrant{ID: svc.ID, Kind: "service", OrgID: "org"}))
	row := &state.Machine{ID: "m_1", Name: "web-1", HostID: "host-a", ServiceID: svc.ID, State: StateRunning}
	must(st.PutMachine(ctx, row))

	must(m.releaseService(ctx, row))

	gone := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, state.ErrNotFound) {
			t.Errorf("%s survived the service's removal (err=%v)", what, err)
		}
	}
	_, err := st.GetServiceSize(ctx, svc.ID)
	gone("the service size", err)
	_, err = st.GetBrokerGrant(ctx, svc.ID)
	gone("the service broker grant", err)
	for _, id := range []string{"rel_1", "rel_2"} {
		_, err = st.GetReleaseSnapshot(ctx, id)
		gone(id+"'s vmstate row", err)
		_, err = st.GetMachineCPU(ctx, id)
		gone(id+"'s cpu pool row", err)
	}
	if rels, err := st.ReleasesFor(ctx, svc.ID); err != nil || len(rels) != 0 {
		t.Errorf("releases survived the service's removal: %v (err=%v)", rels, err)
	}
}
