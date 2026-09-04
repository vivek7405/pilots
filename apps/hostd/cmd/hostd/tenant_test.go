package main

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/state"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// fakeView is a fleetView over a fixed set of rows.
type fakeView struct {
	machines []state.Machine
	hosts    []state.Host
}

func (v fakeView) Machines() []state.Machine { return v.machines }
func (v fakeView) Hosts() []state.Host       { return v.hosts }
func (v fakeView) Services() []state.Service { return nil }

func testLocator(t *testing.T, selfID string) *mesh.Locator {
	t.Helper()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return mesh.NewLocator(selfID, key.PublicKey(), fakeView{})
}

// A row that outlives the machine it describes must not brick the machine
// that inherited its slot.
//
// This is the shape of a real outage. A slot is reused once its previous
// occupant is gone, so a destroy that never replicated leaves two rows
// claiming one slot. The rules are keyed by slot, so both machines' blocks
// are written to the same veth -- and they are not equivalent blocks: a row
// with no app produces an unconditional drop that lands last and shadows the
// app-aware accept written for the slot's real occupant. The live machine
// loses all of its networking while every rule on the host, read on its own,
// looks exactly right.
func TestAStaleRowDoesNotEvictTheSlotsRealOccupant(t *testing.T) {
	const self = "host-a"
	ghost := state.Machine{
		ID: "m-ghost", HostID: self, Slot: 1, App: "", // no app: the isolating form
		State: "running", UpdatedAt: 100,
	}
	live := state.Machine{
		ID: "m-live", HostID: self, Slot: 1, App: "shop",
		State: "running", UpdatedAt: 200,
	}

	for _, order := range [][]state.Machine{{ghost, live}, {live, ghost}} {
		rules := tenantRules(self, fakeView{machines: order}, testLocator(t, self))

		if len(rules.Local) != 1 {
			t.Fatalf("slot 1 produced %d rule blocks; two blocks on one veth "+
				"means the drop shadows the accept", len(rules.Local))
		}
		if rules.Local[0].App != "shop" {
			t.Errorf("slot 1 was filtered for %q, not the live machine's app; "+
				"the live machine is unreachable from its own app",
				rules.Local[0].App)
		}
	}
}

// Two machines on different slots are both kept: the dedup is per slot, not a
// blanket one-machine-per-host.
func TestEveryLocalSlotGetsItsOwnRules(t *testing.T) {
	const self = "host-a"
	view := fakeView{machines: []state.Machine{
		{ID: "m-1", HostID: self, Slot: 1, App: "shop", State: "running"},
		{ID: "m-2", HostID: self, Slot: 2, App: "shop", State: "running"},
		{ID: "m-3", HostID: "host-b", Slot: 1, App: "shop", State: "running"},
	}}
	rules := tenantRules(self, view, testLocator(t, self))

	if len(rules.Local) != 2 {
		t.Fatalf("got %d local blocks, want one per local slot", len(rules.Local))
	}
	// Ordered, so the ruleset is a function of fleet state rather than of map
	// iteration -- otherwise the chain is rewritten in a new order every tick.
	if rules.Local[0].SlotIdx > rules.Local[1].SlotIdx {
		t.Errorf("local rules are not ordered by slot: %v", rules.Local)
	}
}

// The two counters are the same mechanism pointing opposite ways: a suspended
// replica's address gets a counted drop whose rise is a wake, a running one's
// gets a counted pass-through whose rise is activity. A machine with no
// release is in neither, because a sandbox gives its slot and its address back
// when it suspends and has nothing for a peer to address.
func TestARunningReplicaGetsACounterAndASuspendedOneAWakeRule(t *testing.T) {
	const self = "host-a"
	rows := []state.Machine{
		{ID: "m-run", HostID: self, Slot: 3, App: "shop", State: "running", ReleaseID: "rel-1"},
		{ID: "m-susp", HostID: self, Slot: 4, App: "shop", State: "suspended", ReleaseID: "rel-1"},
		{ID: "m-box", HostID: self, Slot: 5, App: "shop", State: "running"},
	}

	rules := tenantRules(self, fakeView{machines: rows}, testLocator(t, self))

	if len(rules.Activity) != 1 || rules.Activity[0].MachineID != "m-run" {
		t.Errorf("activity counters = %v, want only the running replica", rules.Activity)
	}
	if len(rules.Wake) != 1 || rules.Wake[0].MachineID != "m-susp" {
		t.Errorf("wake rules = %v, want only the suspended replica", rules.Wake)
	}
	if !rules.Activity[0].Addr.IsValid() {
		t.Error("the running replica's counter has no address to match on")
	}
}
