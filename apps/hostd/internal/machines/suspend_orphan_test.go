package machines

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A row that says running with no process here is CORRECTED, not answered
// "done".
//
// Suspend read "not in this host's map" as "already suspended or stopped" for
// both of its meanings. A row that genuinely says suspended is done; a row that
// says RUNNING with no process is wrong, and returning nil without touching it
// left the idle monitor to find it running again ten seconds later and suspend
// it again, for ever.
//
// Measured on a host carrying one such row: 29 "machine suspended after going
// idle" lines for one machine in five minutes -- every other line in the log --
// and not one of them changed anything. It is the same retry loop the
// ErrGuestGone branch exists to stop, arriving by a different door.
//
// Restoring the bare `return nil` leaves the state running and reds this.
func TestSuspendingARowWithNoProcessCorrectsIt(t *testing.T) {
	m, st := storeManager(t)
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_orphan", Name: "orphan", HostID: "host-a",
		State: StateRunning, VCPUs: 1, MemMiB: 512,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Suspend(t.Context(), "m_orphan"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	row, err := st.GetMachine(t.Context(), "m_orphan")
	if err != nil {
		t.Fatal(err)
	}
	if row.State == StateRunning {
		t.Error("the row still says running, so the idle monitor will suspend it " +
			"again on its next pass, and on every pass after that")
	}
	// STOPPED, not suspended: suspended promises a memory image to wake from,
	// and a machine whose process is gone has none.
	if row.State != StateStopped {
		t.Errorf("state = %q, want %q", row.State, StateStopped)
	}
}

// A row that already says suspended is left exactly as it was. The fix must
// not rewrite the state of a machine that is legitimately asleep, which would
// throw away the memory image it wakes from.
func TestSuspendingAnAlreadySuspendedRowChangesNothing(t *testing.T) {
	m, st := storeManager(t)
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_asleep", Name: "asleep", HostID: "host-a",
		State: StateSuspended, VCPUs: 1, MemMiB: 512, MemBuildID: "mem-1",
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Suspend(t.Context(), "m_asleep"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	row, err := st.GetMachine(t.Context(), "m_asleep")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateSuspended {
		t.Errorf("state = %q, want it left at %q", row.State, StateSuspended)
	}
	if row.MemBuildID != "mem-1" {
		t.Errorf("the memory image it wakes from was cleared: %q", row.MemBuildID)
	}
}

// And a machine another host holds is still refused rather than corrected
// here: one host does not write another's rows.
func TestSuspendingAForeignRowIsStillRefused(t *testing.T) {
	m, st := storeManager(t)
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_far", Name: "far", HostID: "host-b", State: StateRunning,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Suspend(t.Context(), "m_far"); err == nil {
		t.Fatal("a machine held by another host was suspended here")
	}
	row, err := st.GetMachine(t.Context(), "m_far")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateRunning {
		t.Errorf("another host's row was rewritten to %q", row.State)
	}
}
