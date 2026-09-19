package machines

import (
	"context"
	"testing"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// A zero interval is the operator switching the bound off, and off means no
// ticker at all, not a ticker that does nothing: RunRootFlush returns at once.
func TestRunRootFlushIsOffAtZero(t *testing.T) {
	m := New(Options{HostID: "host-a", RootFlushInterval: 0})

	done := make(chan struct{})
	go func() {
		m.RunRootFlush(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRootFlush kept running with the interval set to zero")
	}
}

// The single-writer guard (AGENTS.md hard rule 1). A flush writes
// rootfs_build_id and mem_build_id on the machine row, so it may only ever
// run on the owning host: another host's rows are not this host's to flush,
// however running they are, and a suspended machine has no disk to flush. A
// builder is hostd's own machine and its layer cache is rebuilt rather than
// restored, so it is left alone too.
func TestRootFlushSelectsOnlyThisHostsRunningMachines(t *testing.T) {
	m := New(Options{HostID: "host-a", RootFlushInterval: time.Minute})

	rows := []state.Machine{
		{ID: "mine-running", HostID: "host-a", State: StateRunning, Name: "web"},
		{ID: "mine-suspended", HostID: "host-a", State: StateSuspended, Name: "db"},
		{ID: "theirs-running", HostID: "host-b", State: StateRunning, Name: "api"},
		{ID: "mine-builder", HostID: "host-a", State: StateRunning, Name: builderNamePrefix + "org-1"},
		{ID: "mine-creating", HostID: "host-a", State: StateCreating, Name: "new"},
	}
	got := m.selectFlushable(rows)
	if len(got) != 1 || got[0] != "mine-running" {
		t.Fatalf("selected %v, want exactly [mine-running]", got)
	}
}

// The flush never queues behind another operation. A machine somebody else
// holds is one whose disk that operation is already making durable -- a
// suspend, a checkpoint, a rollout's own capture -- so the flush takes it with
// TryLock and leaves it for the next tick. Blocking here is what let a
// background timer hold a deploy for the length of an upload.
func TestRootFlushSkipsAMachineAnotherOperationHolds(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()

	row := &state.Machine{ID: "m-1", HostID: "host-a", State: StateRunning,
		VCPUs: 1, MemMiB: 512}
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}

	// Held by "another operation" for longer than this test will wait.
	lock := m.lockFor("m-1")
	lock.Lock()
	defer lock.Unlock()

	done := make(chan struct{})
	go func() {
		m.flushRoot(ctx, "m-1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a flush queued behind the operation holding the machine; " +
			"a deploy's checkpoint would wait for it")
	}
}

// A machine that is not running here -- nothing in the registry -- is not a
// machine this host can pause, and the flush must not touch its row on the
// strength of a listing alone.
func TestRootFlushLeavesAMachineItIsNotRunningAlone(t *testing.T) {
	m, rec, _ := newColdBootManager(t)
	ctx := context.Background()

	row := &state.Machine{ID: "m-1", HostID: "host-a", State: StateRunning,
		VCPUs: 1, MemMiB: 512, MemBuildID: "mem-1", RootfsBuildID: "rootfs-1"}
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	before := len(rec.order())

	m.flushRoot(ctx, "m-1")

	if got := rec.order(); len(got) != before {
		t.Fatalf("a flush of a machine this host is not running wrote %v", got[before:])
	}
	after, err := m.opts.Store.GetMachine(ctx, "m-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.MemBuildID != "mem-1" || after.RootfsBuildID != "rootfs-1" {
		t.Fatalf("the row moved to mem=%q rootfs=%q with no flush having run",
			after.MemBuildID, after.RootfsBuildID)
	}
}

// Flushes are spread across the interval, one due time per machine, and a
// machine is due once per interval however often the loop looks.
func TestFlushesAreSpreadAcrossTheIntervalAndDueOncePerInterval(t *testing.T) {
	interval := time.Minute
	m := New(Options{HostID: "host-a", RootFlushInterval: interval})
	rows := []state.Machine{
		{ID: "m-a", HostID: "host-a", State: StateRunning},
		{ID: "m-b", HostID: "host-a", State: StateRunning},
		{ID: "m-c", HostID: "host-a", State: StateRunning},
	}
	for _, r := range rows {
		if p := flushPhase(r.ID, interval); p < 0 || p >= interval {
			t.Fatalf("phase of %s is %v, outside [0, %v)", r.ID, p, interval)
		}
	}
	if flushTick(interval) >= interval {
		t.Fatalf("the loop looks every %v for a %v interval; nothing can spread", flushTick(interval), interval)
	}

	start := time.Now()
	seen := map[string]int{}
	for tick := time.Duration(0); tick < 2*interval; tick += flushTick(interval) {
		for _, id := range m.dueFlushes(rows, start.Add(tick)) {
			seen[id]++
		}
	}
	for _, r := range rows {
		if seen[r.ID] != 2 {
			t.Errorf("%s was due %d times in two intervals, want 2: %v", r.ID, seen[r.ID], seen)
		}
	}
	// A machine no longer flushable is forgotten, so the map is bounded.
	_ = m.dueFlushes(rows[:1], start.Add(3*interval))
	if _, still := m.flushDue.Load("m-c"); still {
		t.Error("a machine that left the flushable set kept its due time")
	}
}
