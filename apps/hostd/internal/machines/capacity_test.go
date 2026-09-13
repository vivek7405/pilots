package machines

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// capManager is a manager with a memory figure a test controls and a real
// store behind it, which is everything admission reads.
func capManager(t *testing.T, freeMiB, cpus int) (*Manager, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Manager{
		opts: Options{
			HostID: "host-a", Store: st, CPUCount: cpus,
			FreeMemMiB: func() int { return freeMiB },
		},
		flight: newInFlight(),
	}, st
}

// idleMachine is a running machine on this host that has been quiet long
// enough to be reclaimable.
func idleMachine(t *testing.T, st state.Store, id string, memMiB int, quietFor time.Duration) {
	t.Helper()
	knobs, err := json.Marshal(api.Knobs{AutoStop: "suspend", AutoStart: true, IdleTimeout: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: id, Name: id, HostID: "host-a", State: StateRunning,
		VCPUs: 1, MemMiB: memMiB, KindKnobs: string(knobs),
		LastActivity: time.Now().Add(-quietFor).Unix(),
		UpdatedAt:    time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

// The ordinary case, and the one that must stay cheap: the machine fits in
// free memory, so nothing is read, nothing is suspended, and no tenant's
// machine is disturbed to make room that was already there.
func TestAdmissionTakesAMachineThatFits(t *testing.T) {
	m, st := capManager(t, 8192, 8)
	idleMachine(t, st, "m_idle", 2048, time.Hour)

	if err := m.admit(t.Context(), 1, 1024); err != nil {
		t.Fatalf("a 1 GiB machine was refused by a host with 8 GiB free: %v", err)
	}
	// And the idle machine was left alone.
	row, err := st.GetMachine(t.Context(), "m_idle")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateRunning {
		t.Errorf("an idle machine was suspended to make room that already existed (state=%q)", row.State)
	}
}

// A host with no room left and nothing to reclaim refuses, and the refusal is
// the sentinel the API turns into a 507. Before this, nothing on the create
// path read free memory at all: the machine was created, booted, and failed
// with whatever Firecracker said about memory.
func TestAdmissionRefusesWhenThereIsNothingToReclaim(t *testing.T) {
	m, _ := capManager(t, 512, 8)

	err := m.admit(t.Context(), 1, 8192)
	if !errors.Is(err, api.ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity so the API answers 507", err)
	}
}

// More vCPUs than the host physically has is refused rather than started.
// Firecracker will happily start it, and the guest then contends with itself
// for ever, which reads as an application that is mysteriously slow.
func TestAdmissionRefusesMoreVCPUsThanTheHostHas(t *testing.T) {
	m, _ := capManager(t, 65536, 4)

	if err := m.admit(t.Context(), 16, 1024); !errors.Is(err, api.ErrNoCapacity) {
		t.Errorf("err = %v; a 16-vCPU machine on a 4-CPU host must be refused", err)
	}
	if err := m.admit(t.Context(), 4, 1024); err != nil {
		t.Errorf("a machine using every CPU was refused: %v", err)
	}
}

// A draining host takes nothing. Without this the drain chases its own tail:
// it moves machines off while the placer puts new ones back on.
func TestADrainingHostAdmitsNothing(t *testing.T) {
	m, _ := capManager(t, 65536, 8)
	m.SetDraining(true)

	if err := m.admit(t.Context(), 1, 512); !errors.Is(err, api.ErrNoCapacity) {
		t.Errorf("err = %v; a draining host must admit nothing", err)
	}
	m.SetDraining(false)
	if err := m.admit(t.Context(), 1, 512); err != nil {
		t.Errorf("undraining did not restore admission: %v", err)
	}
}

// What counts as reclaimable is the idle monitor's own rules with the WAIT
// treated as elapsed. Everything that makes a machine ineligible for suspend
// makes it ineligible here, or admission would suspend a machine the idle
// monitor had already decided to leave alone.
func TestWhatIsAndIsNotReclaimable(t *testing.T) {
	knobsOf := func(t *testing.T, k api.Knobs) string {
		t.Helper()
		blob, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		return string(blob)
	}

	for _, tc := range []struct {
		name string
		row  state.Machine
		want bool
	}{
		{
			"an idle suspendable machine",
			state.Machine{KindKnobs: knobsOf(t, api.Knobs{AutoStop: "suspend"}),
				LastActivity: time.Now().Add(-time.Hour).Unix()},
			true,
		},
		{
			"auto_stop off: the tenant asked for it to stay up",
			state.Machine{KindKnobs: knobsOf(t, api.Knobs{AutoStop: "off"}),
				LastActivity: time.Now().Add(-time.Hour).Unix()},
			false,
		},
		{
			"a warm floor: the tenant is paying to keep it warm",
			state.Machine{KindKnobs: knobsOf(t, api.Knobs{AutoStop: "suspend", MinMachinesRunning: 1}),
				LastActivity: time.Now().Add(-time.Hour).Unix()},
			false,
		},
		{
			"quiet for less than the grace period",
			state.Machine{KindKnobs: knobsOf(t, api.Knobs{AutoStop: "suspend"}),
				LastActivity: time.Now().Add(-time.Second).Unix()},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := capManager(t, 1024, 8)
			tc.row.ID = "m_x"
			if got := m.reclaimableNow(t.Context(), tc.row); got != tc.want {
				t.Errorf("reclaimableNow = %v, want %v", got, tc.want)
			}
		})
	}

	// A request in flight is activity, whatever the clock says.
	t.Run("a request in flight", func(t *testing.T) {
		m, _ := capManager(t, 1024, 8)
		row := state.Machine{ID: "m_busy",
			KindKnobs:    knobsOf(t, api.Knobs{AutoStop: "suspend"}),
			LastActivity: time.Now().Add(-time.Hour).Unix()}
		m.flight.begin(row.ID)
		defer m.flight.end(row.ID)
		if m.reclaimableNow(t.Context(), row) {
			t.Error("a machine with a request in flight was called reclaimable")
		}
	})
}

// The reported figure is what placement ranks on, so it must count only this
// host's running machines and must not count a machine it would never suspend.
func TestReportedCapacityCountsOnlyWhatItCouldFree(t *testing.T) {
	m, st := capManager(t, 4096, 8)
	ctx := t.Context()

	idleMachine(t, st, "m_idle", 2048, time.Hour)

	// Pinned: the tenant asked for it to stay up.
	pinned, err := json.Marshal(api.Knobs{AutoStop: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_pinned", Name: "pinned", HostID: "host-a", State: StateRunning,
		VCPUs: 2, MemMiB: 4096, KindKnobs: string(pinned),
		LastActivity: time.Now().Add(-time.Hour).Unix(), UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	// Another host's machine is none of this host's business.
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_elsewhere", Name: "elsewhere", HostID: "host-b", State: StateRunning,
		VCPUs: 4, MemMiB: 8192, UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	got := m.Capacity(ctx)
	if got == nil {
		t.Fatal("no capacity reported")
	}
	if got.MemFreeMiB != 4096 {
		t.Errorf("free = %d, want 4096", got.MemFreeMiB)
	}
	if got.MemReclaimableMiB != 2048 {
		t.Errorf("reclaimable = %d, want only the idle machine's 2048", got.MemReclaimableMiB)
	}
	if got.VCPUsRunning != 3 {
		t.Errorf("vcpus_running = %d, want 3 (this host's two machines)", got.VCPUsRunning)
	}
	if got.Headroom() != 6144 {
		t.Errorf("headroom = %d, want 6144", got.Headroom())
	}
}

// A suspended machine holds NO memory: suspend kills the Firecracker process
// after taking the snapshot. Counting one as reclaimable would double-count
// memory that is already free and admit creates that then fail to boot.
func TestASuspendedMachineIsNotCountedAsReclaimable(t *testing.T) {
	m, st := capManager(t, 4096, 8)

	knobs, err := json.Marshal(api.Knobs{AutoStop: "suspend"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_asleep", Name: "asleep", HostID: "host-a", State: StateSuspended,
		VCPUs: 1, MemMiB: 8192, KindKnobs: string(knobs),
		LastActivity: time.Now().Add(-time.Hour).Unix(), UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	got := m.Capacity(t.Context())
	if got == nil {
		t.Fatal("no capacity reported")
	}
	if got.MemReclaimableMiB != 0 {
		t.Errorf("reclaimable = %d, want 0: a suspended machine's memory is already free",
			got.MemReclaimableMiB)
	}
}

// A host that cannot measure its own memory must not refuse every create.
// Before admission existed every create was admitted; a blipped read of
// /proc/meminfo must not be worse than that.
func TestAHostThatCannotMeasureItselfStillAdmits(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &Manager{opts: Options{HostID: "host-a", Store: st}, flight: newInFlight()}

	if err := m.admit(t.Context(), 4, 65536); err != nil {
		t.Errorf("a host with no memory reading refused a create: %v", err)
	}
}
