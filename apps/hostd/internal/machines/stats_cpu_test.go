package machines

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// cpuManager is a manager whose state dir and cgroup root are both temporary,
// so PersistCPU can be driven for real rather than simulated.
func cpuManager(t *testing.T, id string) (*Manager, string) {
	t.Helper()
	cgRoot := t.TempDir()
	old := cgroupRoot
	cgroupRoot = cgRoot
	t.Cleanup(func() { cgroupRoot = old })

	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	row := &state.Machine{
		ID: id, Name: id, HostID: "host-a",
		State: StateRunning, VCPUs: 1, MemMiB: 256,
	}
	if err := st.PutMachine(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opts: Options{
		HostID: "host-a", Store: st, StateRoot: t.TempDir(),
		FCConfig: fc.Config{FirecrackerBin: "/usr/bin/firecracker"},
	}}
	return m, filepath.Join(cgRoot, "pilots", "firecracker", id)
}

// writeCPUStat puts a cumulative counter in the machine's slice, creating the
// directory if it is not there.
func writeCPUStat(t *testing.T, dir string, usec int64, procs string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "usage_usec " + strconv.FormatInt(usec, 10) + "\nuser_usec 1\nsystem_usec 1\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(procs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Persisting twice against one cgroup does not charge the machine twice.
//
// The cgroup OUTLIVES the machine's processes: suspend kills the VMM and
// leaves the slice in place, and only Destroy and the reaper remove it. So the
// second PersistCPU read the SAME cumulative counter out of the SAME file and
// added it to a total that already contained it. Suspend then stop, suspend
// then checkpoint, or a suspend retried after a transient failure each doubled
// the machine's recorded CPU, and every further repeat adds that whole period
// again on top.
//
// This is a billing number. Dropping the fold check makes the 100 s below read
// back as 200 s and then 300 s, which is what this test measured.
func TestPersistingTwiceDoesNotChargeTwice(t *testing.T) {
	m, cgDir := cpuManager(t, "m_cpu1")
	writeCPUStat(t, cgDir, 100_000_000, "4242\n") // 100 s

	m.PersistCPU("m_cpu1")
	if got := m.carriedCPU("m_cpu1"); got != 100_000_000 {
		t.Fatalf("after one persist, carried = %d, want 100000000", got)
	}
	// Suspend has run; the VMM is gone but the slice, and its counter, are not.
	writeCPUStat(t, cgDir, 100_000_000, "")
	m.PersistCPU("m_cpu1")
	if got := m.carriedCPU("m_cpu1"); got != 100_000_000 {
		t.Errorf("after two persists against one cgroup, carried = %d, want 100000000: "+
			"the machine was charged twice for its last waking period", got)
	}
	m.PersistCPU("m_cpu1")
	if got := m.carriedCPU("m_cpu1"); got != 100_000_000 {
		t.Errorf("after three persists, carried = %d: the error compounds", got)
	}
}

// A cgroup that was removed and recreated is a NEW waking period, and its
// reading is added to the running total rather than replacing it.
//
// The counterpart of the test above, and the reason the discriminator is the
// inode rather than the path: a machine that suspends, wakes and suspends
// again must accumulate.
func TestANewCgroupAccumulatesRatherThanReplaces(t *testing.T) {
	m, cgDir := cpuManager(t, "m_cpu2")
	writeCPUStat(t, cgDir, 100_000_000, "4242\n")
	m.PersistCPU("m_cpu2")

	// Wake: the slice is destroyed and made again, so the counter restarts.
	if err := os.RemoveAll(cgDir); err != nil {
		t.Fatal(err)
	}
	writeCPUStat(t, cgDir, 30_000_000, "4243\n") // 30 s in the new period
	m.PersistCPU("m_cpu2")

	if got := m.carriedCPU("m_cpu2"); got != 130_000_000 {
		t.Errorf("carried = %d, want 130000000: a recreated cgroup is a new period "+
			"and its time is added to the total, not swapped for it", got)
	}
}

// A live Stats reading adds only the part of the counter nothing has folded in
// yet. Checkpoint persists without ending the machine, so the running branch
// had the same double-count as the write path.
func TestStatsDoesNotReAddWhatWasAlreadyPersisted(t *testing.T) {
	m, cgDir := cpuManager(t, "m_cpu3")
	writeCPUStat(t, cgDir, 100_000_000, "4242\n")
	m.PersistCPU("m_cpu3") // a checkpoint, machine still running

	// It keeps running and reaches 150 s.
	writeCPUStat(t, cgDir, 150_000_000, "4242\n")
	got, err := m.Stats(t.Context(), "m_cpu3")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.CPUSeconds != 150 {
		t.Errorf("CPUSeconds = %v, want 150: the persisted 100 s is part of the "+
			"counter's own 150, not something to add to it", got.CPUSeconds)
	}
}

// A record written before the fold fields existed keeps the old behaviour of
// adding. Under-reporting somebody's accumulated total on an upgrade would be
// the worse direction, and FoldedIno 0 never matches a real inode.
func TestAPreUpgradeRecordStillAccumulates(t *testing.T) {
	m, cgDir := cpuManager(t, "m_cpu4")
	writeCarried(t, m, "m_cpu4", 90_000_000) // the old one-field shape
	writeCPUStat(t, cgDir, 10_000_000, "4242\n")

	got, err := m.Stats(t.Context(), "m_cpu4")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.CPUSeconds != 100 {
		t.Errorf("CPUSeconds = %v, want 100: an old record has nothing folded in "+
			"and its total still stands", got.CPUSeconds)
	}
}

// A recreated cgroup that landed on its predecessor's inode still accumulates.
//
// kernfs allocates inode numbers from an ida that can hand a freed number
// back, so the inode alone is not proof of identity. cpu.stat is monotonic
// within one cgroup, so a reading BELOW what was folded says the counter
// restarted whatever the inode says. Without that second guard a reused inode
// would silently discard a whole waking period.
func TestAReusedInodeWithARestartedCounterAccumulates(t *testing.T) {
	m, cgDir := cpuManager(t, "m_cpu5")
	writeCPUStat(t, cgDir, 100_000_000, "4242\n")
	m.PersistCPU("m_cpu5")

	// Forge the worst case directly: the slice was recreated and the counter
	// restarted, but the record still names this directory's inode.
	saved, ok := m.savedStats("m_cpu5")
	if !ok {
		t.Fatal("no persisted record")
	}
	if saved.FoldedIno != cgroupIno(cgDir) {
		t.Fatalf("FoldedIno = %d, want this directory's inode", saved.FoldedIno)
	}
	writeCPUStat(t, cgDir, 20_000_000, "4243\n") // restarted, now at 20 s
	m.PersistCPU("m_cpu5")

	if got := m.carriedCPU("m_cpu5"); got != 120_000_000 {
		t.Errorf("carried = %d, want 120000000: a counter that went backwards is a "+
			"new period however the inode compares", got)
	}
}
