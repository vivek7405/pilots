package machines

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeCgroup writes one machine's slice with the files Stats reads.
//
// procs is what goes in cgroup.procs: a pid for a machine that is running,
// empty for one whose VMM is gone but whose slice the kernel still holds.
func fakeCgroup(t *testing.T, id, procs string, current, max int64) {
	t.Helper()
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	dir := filepath.Join(root, "pilots", "firecracker", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cgroup.procs", procs)
	write("cpu.stat", "usage_usec 2269379\nuser_usec 1\nsystem_usec 1\n")
	write("memory.current", strconv.FormatInt(current, 10))
	write("memory.max", strconv.FormatInt(max, 10))
}

// statsManager is a manager owning one machine row, with the cgroup layout
// pointed at whatever fakeCgroup just built.
func statsManager(t *testing.T, id string, memMiB int) *Manager {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	row := &state.Machine{
		ID: id, Name: id, HostID: "host-a",
		State: StateRunning, VCPUs: 1, MemMiB: memMiB,
	}
	if err := st.PutMachine(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return &Manager{opts: Options{
		HostID: "host-a", Store: st,
		FCConfig: fc.Config{FirecrackerBin: "/usr/bin/firecracker"},
	}}
}

// A machine's memory limit is the RAM it was asked for, not the cgroup's.
//
// # The bug
//
// The cgroup's memory.max is the guest's RAM PLUS 128 MiB of headroom for
// Firecracker's own allocations, and Stats preferred it. So the owner of a
// 512 MiB machine was told their limit was 640 MiB -- 128 MiB of hypervisor
// slice their guest can never allocate, having been OOM-killed at 512.
//
// It was wrong downstream too: the CLI warns at 90% of the limit, which
// against 640 MiB is 576 MiB and therefore never, and the dashboard's fill bar
// showed a full machine at 80%.
func TestTheMemoryLimitIsTheGuestsRAMNotTheCgroupsCeiling(t *testing.T) {
	const memMiB = 512
	// What the jailer actually writes: 512 + 128 MiB of VMM overhead.
	fakeCgroup(t, "m_abc", "4242\n", 27271168, (memMiB+vmmOverheadMiB)<<20)

	m := statsManager(t, "m_abc", memMiB)
	got, err := m.Stats(t.Context(), "m_abc")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if want := int64(memMiB) << 20; got.MemoryLimitBytes != want {
		t.Errorf("memory_limit_bytes = %d, want %d. The extra is Firecracker's "+
			"own headroom, which is not memory the guest can use",
			got.MemoryLimitBytes, want)
	}
}

// A machine whose processes are gone is using no memory.
//
// # The bug
//
// Stats assumed "no cgroup means suspended", and nothing on the suspend path
// removes the cgroup -- only Destroy and the reaper do. So the slice survived,
// memory.current still reported the page cache and slab the kernel had not
// reclaimed, and a suspended machine read as a small running one. On the rig
// that was 4.6 MiB.
func TestASuspendedMachineReportsNoMemory(t *testing.T) {
	// The slice is still there, still charged, and has no processes in it.
	fakeCgroup(t, "m_abc", "\n", 4820992, 640<<20)

	m := statsManager(t, "m_abc", 512)
	got, err := m.Stats(t.Context(), "m_abc")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.MemoryBytes != 0 {
		t.Errorf("a machine with an empty cgroup.procs reported %d bytes of "+
			"memory; with no process there is nothing using memory, and a "+
			"residual charge reads exactly like a small machine running",
			got.MemoryBytes)
	}
	// The CPU total still comes back, because it is a counter and a suspend
	// must never lower it.
	if got.CPUSeconds <= 0 {
		t.Errorf("cpu_seconds = %v; a suspend must not lose the total", got.CPUSeconds)
	}
}

// A running machine still reports what it is using, or the fix above would be
// indistinguishable from always reporting zero.
func TestARunningMachineStillReportsItsMemory(t *testing.T) {
	fakeCgroup(t, "m_abc", "4242\n", 27271168, 640<<20)

	m := statsManager(t, "m_abc", 512)
	got, err := m.Stats(t.Context(), "m_abc")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.MemoryBytes != 27271168 {
		t.Errorf("memory_bytes = %d, want the cgroup's 27271168", got.MemoryBytes)
	}
}
