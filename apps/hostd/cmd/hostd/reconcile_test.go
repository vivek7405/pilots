package main

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A dead machine must not be settled until every live one holds its slot.
//
// ExitedWhileDown runs onExit synchronously, and onExit can bring a machine
// back in place. A restart run inside the adoption loop takes its netns index
// from a pool that has not yet seen the machines further down the scan, so it
// can be handed an index a live guest is serving in -- and that machine's own
// Adopt then fails outright, leaving a running Firecracker with no registry
// entry, no discovery binding, no metering, and nothing that will ever tear it
// down.
//
// The fixture is the stale-breadcrumb case the rest of this engine already
// defends against: a machine that died while hostd was down leaves a state.json
// naming an index that is now somebody else's. `isFirecracker` and
// `ClearBreadcrumbs` exist because a breadcrumb is not evidence; this is the
// same hazard reaching the slot pool.
func TestALiveMachineIsAdoptedBeforeADeadOneIsSettled(t *testing.T) {
	ctx := context.Background()

	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mgr := machines.New(machines.Options{
		HostID: "host-a", Store: st, Vendor: "AuthenticAMD",
		StateRoot: t.TempDir(), CacheRoot: t.TempDir(),
	})

	// The dead one is a service replica, so settleExit KEEPS its slot index
	// rather than handing it straight back. That is the production rule: a
	// replica stays addressable while it is down.
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m-aaa", Name: "m-aaa", HostID: "host-a", State: "running",
		ServiceID: "svc-1", ReleaseID: "rel-1", VCPUs: 1, MemMiB: 512, Slot: 1,
		// Named so templateFor resolves against a build this host cannot
		// fetch and fails fast, rather than falling back to EnsureTemplate
		// and photographing a golden image inside a unit test.
		TemplateMemBuildID:    uuid.NewString(),
		TemplateRootfsBuildID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m-zzz", Name: "m-zzz", HostID: "host-a", State: "running",
		VCPUs: 1, MemMiB: 512, Slot: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// A real live pid for the survivor: the watcher put subscribes takes a
	// pidfd on it, and a pid that is already gone would fire an exit the
	// moment it is adopted.
	live := exec.Command("sleep", "60")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := live.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		// Out of the registry before the kill, so the exit watcher does not
		// drive a reaction against a store this test is about to close.
		_ = mgr.StopLocal(ctx, "m-zzz")
		_ = syscall.Kill(-live.Process.Pid, syscall.SIGKILL)
	})

	// Dead first, which is what ReadDir gives for these two names.
	found := []fc.Reconciled{
		{State: fc.State{MachineID: "m-aaa", Pid: 999999, SlotIdx: 1}, Alive: false},
		{State: fc.State{MachineID: "m-zzz", Pid: live.Process.Pid, SlotIdx: 1}, Alive: true},
	}

	if got := settleReconciled(found, t.TempDir(), mgr, nil); got != 1 {
		t.Errorf("settleReconciled adopted %d machines, want the one that is still running", got)
	}

	if !adoptedHolds(mgr.Running(), "m-zzz") {
		t.Errorf("the registry holds %v: the live machine was never adopted, because "+
			"its slot was taken by a machine that is already dead. It has no "+
			"registry entry, no discovery binding, no metering, and nothing that "+
			"will ever tear it down", mgr.Running())
	}

	// And the dead one was still settled rather than skipped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := st.GetMachine(ctx, "m-aaa")
		if err == nil && row.State == "error" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, err := st.GetMachine(ctx, "m-aaa")
	if err != nil {
		t.Fatal(err)
	}
	t.Errorf("the dead machine's row says %q, want error", row.State)
}

func adoptedHolds(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
