package fc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{
		MachineID: "m_1", Pid: 4242, SlotIdx: 7, MAC: "02:00:00:00:00:01",
		ChrootDir: "/srv/jailer/firecracker/m_1/root", NetnsName: "m_1",
		StartedAtNs: time.Now().UnixNano(),
	}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// The pid is stored separately so a liveness check never depends on
	// parsing the state file.
	pid, err := ReadPid(dir)
	if err != nil {
		t.Fatalf("ReadPid: %v", err)
	}
	if pid != want.Pid {
		t.Errorf("ReadPid = %d, want %d", pid, want.Pid)
	}
}

// A crash partway through a direct write would leave a truncated file that
// never parses again, permanently orphaning the machine. The write must be
// atomic, and it must not leave its temp file behind.
func TestWriteStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{MachineID: "m_1", Pid: 1}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile+".tmp")); !os.IsNotExist(err) {
		t.Error("temp file survived the write")
	}

	// Overwriting must fully replace, not merge.
	if err := WriteState(dir, State{MachineID: "m_1", Pid: 2, SlotIdx: 9}); err != nil {
		t.Fatalf("second WriteState: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.Pid != 2 || got.SlotIdx != 9 {
		t.Errorf("overwrite did not take: %+v", got)
	}
}

// While fc.pid exists, reconcile believes the machine is live -- so a destroy
// that fails to remove it means the next hostd start resurrects a machine the
// user deliberately deleted.
func TestClearBreadcrumbsRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{MachineID: "m_1", Pid: 1}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := ClearBreadcrumbs(dir); err != nil {
		t.Fatalf("ClearBreadcrumbs: %v", err)
	}
	for _, f := range []string{pidFile, stateFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived", f)
		}
	}
	// Idempotent: destroy can run twice.
	if err := ClearBreadcrumbs(dir); err != nil {
		t.Errorf("second ClearBreadcrumbs: %v", err)
	}
}

// The pid alone is not enough to adopt a process: pids get recycled, and a
// stale breadcrumb can name a pid the kernel has since given to something
// else. Adopting that would send lifecycle signals to an innocent process.
func TestLiveProcessRejectsNonFirecracker(t *testing.T) {
	// This test binary is alive but is not Firecracker.
	if got := LiveProcess(os.Getpid()); got != 0 {
		t.Errorf("LiveProcess adopted a non-firecracker process (%d)", got)
	}
	if got := LiveProcess(0); got != 0 {
		t.Errorf("LiveProcess(0) = %d, want 0", got)
	}
	if got := LiveProcess(-1); got != 0 {
		t.Errorf("LiveProcess(-1) = %d, want 0", got)
	}
	// A pid that almost certainly does not exist.
	if got := LiveProcess(4194303); got != 0 {
		t.Errorf("LiveProcess on a dead pid = %d, want 0", got)
	}
}

func TestReconcileReportsLiveness(t *testing.T) {
	root := t.TempDir()

	// A machine whose pid is long gone.
	dead := filepath.Join(root, "m_dead")
	if err := WriteState(dead, State{MachineID: "m_dead", Pid: 4194303}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	// A machine whose pid is this test process: alive, but not Firecracker,
	// so it must NOT be adopted.
	notFC := filepath.Join(root, "m_notfc")
	if err := WriteState(notFC, State{MachineID: "m_notfc", Pid: os.Getpid()}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, err := Reconcile(root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2", len(got))
	}
	for _, r := range got {
		if r.Alive {
			t.Errorf("%s reported alive; neither pid is a live firecracker", r.State.MachineID)
		}
	}
}

func TestReconcileOnMissingRootIsEmpty(t *testing.T) {
	got, err := Reconcile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Reconcile on a missing root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d machines, want 0", len(got))
	}
}

// A machine with an unreadable state file but a live pid still has to be
// reported, or a crashed hostd leaks a running Firecracker nobody owns.
func TestReconcileFallsBackToPidFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "m_corrupt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pidFile), []byte("4194303\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Reconcile(root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The pid is dead, so nothing is reported -- but crucially Reconcile did
	// not fail on the corrupt file.
	if len(got) != 0 {
		t.Errorf("got %d, want 0 (dead pid)", len(got))
	}
}

func TestGenerateMACIsLocallyAdministered(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		mac, err := GenerateMAC()
		if err != nil {
			t.Fatalf("GenerateMAC: %v", err)
		}
		if len(mac) != 17 || mac[:3] != "02:" {
			t.Fatalf("mac %q is not a locally administered unicast address", mac)
		}
		if seen[mac] {
			t.Fatalf("duplicate mac %q after %d draws", mac, i)
		}
		seen[mac] = true
	}
}

// With cgroup v2 and no --cgroup at all, --parent-cgroup changes meaning and
// the jailer fails on any cgroup with domain controllers enabled. There must
// always be at least one.
func TestCgroupArgsAlwaysNonEmpty(t *testing.T) {
	if got := cgroupArgs(Limits{}); len(got) == 0 {
		t.Error("no cgroup args for empty limits; the jailer would fail")
	}
	got := cgroupArgs(Limits{CPUMax: "200000 100000", MemMaxB: 536870912, PidsMax: 512})
	if len(got) != 3 {
		t.Errorf("got %v, want cpu.max, memory.max and pids.max", got)
	}
}
