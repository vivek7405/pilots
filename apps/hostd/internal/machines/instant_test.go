package machines

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// takeSlot is where every local bring-up gets its netns index, and it is the
// only place a kept reservation is consumed. The paths that reach it -- Wake,
// Rescue, a checkpoint rollback, a redeploy -- all need a Firecracker host and
// a real snapshot before they run a line, so the behaviour is pinned here, on
// the helper, against a real pool. The pool is in-memory map work and needs no
// KVM.

// slotManager builds a manager with a real, small pool and nothing else.
func slotManager(t *testing.T, hostID string, size int) *Manager {
	t.Helper()
	return New(Options{HostID: hostID, PoolSize: size})
}

// A replica that suspends here keeps its index on the row and in the pool, and
// every wake has to take that same index back. Ten cycles on a pool of eight
// is deliberate: a pool of size 8 hands out 7 indices, so without the reuse
// this runs out and fails on the error as well as on the count.
func TestAKeptSlotIsReusedAcrossWakesAndThePoolStaysFlat(t *testing.T) {
	m := slotManager(t, "host-a", 8)
	row := &state.Machine{ID: "m_1", HostID: "host-a"}

	first, err := m.takeSlot(row)
	if err != nil {
		t.Fatalf("the create could not take a slot: %v", err)
	}
	if first.Idx <= 0 {
		t.Fatalf("a create was handed index %d, which is the unallocated sentinel", first.Idx)
	}
	if got := m.pool.InUse(); got != 1 {
		t.Fatalf("after one create the pool holds %d indices, want 1", got)
	}

	// What Suspend does for a replica: it returns nothing to the pool and
	// leaves the index on the row.
	row.Slot = first.Idx

	for cycle := 1; cycle <= 10; cycle++ {
		woken, err := m.takeSlot(row)
		if err != nil {
			t.Fatalf("wake %d could not take a slot: %v", cycle, err)
		}
		if woken.Idx != first.Idx {
			t.Fatalf("wake %d came up in slot %d, want the kept %d: the replica's "+
				"mesh address moved", cycle, woken.Idx, first.Idx)
		}
		if got := m.pool.InUse(); got != 1 {
			t.Fatalf("after wake %d the pool holds %d indices, want 1: a slot "+
				"leaked per wake", cycle, got)
		}
		row.Slot = woken.Idx
	}
}

// A redeploy of a SUSPENDED replica is the second way a machine comes up on
// the index it kept, and it is the one that needs two things to be true at
// once: the store must be told the machine holds no namespace while the new
// image boots, and the row the boot reads must still name the index. That is
// what withoutSlot separates, and it is why the row is not stamped to zero
// before the boot. The ordering half is pinned in callsites_test.go.
func TestARedeployOfASuspendedReplicaBootsIntoItsKeptSlot(t *testing.T) {
	m := slotManager(t, "host-a", 8)
	row := &state.Machine{ID: "m_1", HostID: "host-a"}

	first, err := m.takeSlot(row)
	if err != nil {
		t.Fatalf("the create could not take a slot: %v", err)
	}
	row.Slot = first.Idx // suspended, index kept

	for deploy := 1; deploy <= 5; deploy++ {
		// The interim write Redeploy makes before the boot.
		stored := withoutSlot(row)
		if stored.Slot != 0 {
			t.Fatalf("deploy %d wrote slot %d to the store, want 0: a creating "+
				"machine advertises an address nothing is listening on",
				deploy, stored.Slot)
		}
		if row.Slot != first.Idx {
			t.Fatalf("deploy %d lost the kept index off the boot row: %d, want %d",
				deploy, row.Slot, first.Idx)
		}

		// The boot itself, which is bootMachine's call.
		booted, err := m.takeSlot(row)
		if err != nil {
			t.Fatalf("deploy %d could not take a slot: %v", deploy, err)
		}
		if booted.Idx != first.Idx {
			t.Fatalf("deploy %d booted into slot %d, want the kept %d", deploy,
				booted.Idx, first.Idx)
		}
		if got := m.pool.InUse(); got != 1 {
			t.Fatalf("after deploy %d the pool holds %d indices, want 1: a slot "+
				"leaked per deploy", deploy, got)
		}
		row.Slot = booted.Idx
	}
}

// A create's row names no slot, so it takes a fresh one exactly as it did
// before. Index 0 is the unallocated sentinel and is never handed out.
func TestACreateWithNoSlotTakesAFreshOne(t *testing.T) {
	m := slotManager(t, "host-a", 8)

	for i, id := range []string{"m_1", "m_2", "m_3"} {
		row := &state.Machine{ID: id, HostID: "host-a"}
		slot, err := m.takeSlot(row)
		if err != nil {
			t.Fatalf("create %s could not take a slot: %v", id, err)
		}
		if slot.Idx <= 0 {
			t.Fatalf("create %s was handed index %d", id, slot.Idx)
		}
		if got, want := m.pool.InUse(), i+1; got != want {
			t.Fatalf("after %d creates the pool holds %d indices, want %d",
				i+1, got, want)
		}
	}
}

// The index on a row indexes the OWNING host's pool. A rescued row names
// another host until the claim rewrites it, and the same number here may
// belong to a completely different machine, so it is never reserved.
func TestAForeignHostsSlotIsNeverReserved(t *testing.T) {
	m := slotManager(t, "host-a", 8)
	if _, err := m.pool.Reserve(3, "other"); err != nil {
		t.Fatalf("could not set up the local holder of index 3: %v", err)
	}

	row := &state.Machine{ID: "m_2", HostID: "host-b", Slot: 3}
	slot, err := m.takeSlot(row)
	if err != nil {
		t.Fatalf("a rescued row could not take a slot: %v", err)
	}
	if slot.Idx == 3 {
		t.Fatal("a foreign host's index 3 was reserved locally, where it belongs " +
			"to another machine")
	}
	// Reserve is idempotent for its own holder, so this proves "other" was
	// never displaced.
	if _, err := m.pool.Reserve(3, "other"); err != nil {
		t.Fatalf("index 3 no longer belongs to its local holder: %v", err)
	}
	if got := m.pool.InUse(); got != 2 {
		t.Fatalf("the pool holds %d indices, want 2", got)
	}
}

// The after-a-restart case: the pool is rebuilt from the breadcrumbs of
// RUNNING machines only, so a sleeping replica's kept index can be gone or
// held by something else. The bring-up falls back to a fresh index and logs,
// rather than failing over bookkeeping.
func TestAReservationThePoolRefusesFallsBackToAFreshSlot(t *testing.T) {
	m := slotManager(t, "host-a", 8)
	if _, err := m.pool.Reserve(3, "other"); err != nil {
		t.Fatalf("could not set up the local holder of index 3: %v", err)
	}

	row := &state.Machine{ID: "m_2", HostID: "host-a", Slot: 3}
	slot, err := m.takeSlot(row)
	if err != nil {
		t.Fatalf("a refused reservation failed the bring-up instead of falling "+
			"back: %v", err)
	}
	if slot.Idx == 3 {
		t.Fatal("index 3 was handed out twice")
	}
	if _, err := m.pool.Reserve(3, "other"); err != nil {
		t.Fatalf("index 3 no longer belongs to its local holder: %v", err)
	}
	if got := m.pool.InUse(); got != 2 {
		t.Fatalf("the pool holds %d indices, want 2", got)
	}
}
