package machines

import (
	"context"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A suspended machine keeps its slot, and so its mesh address, across a
// hostd restart. Adoption only finds processes, so the reservation has to be
// rebuilt from the rows; without it the next create was handed a sleeping
// machine's slot and sat behind its wake trap.
func TestASuspendedMachinesSlotSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, row := range []*state.Machine{
		{ID: "m-asleep", Name: "asleep", HostID: "host-a", State: "suspended", Slot: 3},
		{ID: "m-foreign", Name: "foreign", HostID: "host-b", State: "suspended", Slot: 4},
		{ID: "m-gone", Name: "gone", HostID: "host-a", State: "destroyed", Slot: 5},
		{ID: "m-twin", Name: "twin", HostID: "host-a", State: "suspended", Slot: 3},
	} {
		if err := st.PutMachine(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Options{HostID: "host-a", PoolSize: 8, Store: st})

	n, err := m.ReserveHeldSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-reserved %d slots, want 1: the sleeping machine's, not a "+
			"foreign host's, a destroyed row's, or a second claimant's", n)
	}
	if _, err := m.pool.Reserve(3, "someone-else"); err == nil {
		t.Fatal("slot 3 was handed to another machine; the sleeping machine lost its address")
	}
	for _, idx := range []int{4, 5} {
		if _, err := m.pool.Reserve(idx, "someone-else"); err != nil {
			t.Fatalf("slot %d was reserved although no sleeping machine here holds it: %v", idx, err)
		}
	}

	fresh, err := m.takeSlot(&state.Machine{ID: "m-new", HostID: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Idx == 3 {
		t.Fatal("a create was handed the sleeping machine's slot")
	}
}
