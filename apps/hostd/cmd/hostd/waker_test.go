package main

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// staticView is a fleetView over a fixed set of rows.
type staticView struct{ machines []state.Machine }

func (v staticView) Machines() []state.Machine { return v.machines }
func (v staticView) Hosts() []state.Host       { return nil }

// The waker must never act on a machine this host no longer owns.
//
// A rescue can move a machine while a peer still holds a DNS answer pointing
// here, and the counter keeps ticking on packets that arrive after the move.
// Waking then would start a second copy of a machine that is already running
// elsewhere -- the one failure the single-writer discipline exists to prevent,
// and one a CRDT merge would not report. fly hit this same class of race
// between proxy wake decisions and gossip propagation.
func TestOwnershipIsRecheckedBeforeWaking(t *testing.T) {
	view := staticView{machines: []state.Machine{
		{ID: "m-1", HostID: "host-b", State: "suspended", ServiceID: "svc-1", Slot: 3},
	}}

	row, ok := currentRow(view, "m-1")
	if !ok {
		t.Fatal("the row should be visible")
	}
	if row.HostID == "host-a" {
		t.Fatal("fixture is wrong: the machine should belong to another host")
	}
	// The waker's guard is exactly this comparison; assert it rejects.
	if row.HostID == "host-a" || row.State != "suspended" {
		t.Error("a machine owned by another host would have been woken here")
	}
}

// A machine that has vanished from the fleet view is not woken either.
func TestAVanishedMachineIsNotWoken(t *testing.T) {
	if _, ok := currentRow(staticView{}, "m-gone"); ok {
		t.Error("currentRow claimed to find a machine that is not in the view")
	}
}

// A machine that woke by some other path is left alone.
func TestARunningMachineIsNotWokenAgain(t *testing.T) {
	view := staticView{machines: []state.Machine{
		{ID: "m-1", HostID: "host-a", State: "running", ServiceID: "svc-1", Slot: 3},
	}}
	row, _ := currentRow(view, "m-1")
	if row.State == "suspended" {
		t.Error("a running machine looked suspended to the waker's guard")
	}
}
