package machines

import (
	"os"
	"path/filepath"
	"testing"
)

// A fake cgroup tree, so the parser is exercised against the shape the kernel
// actually writes rather than against a string somebody typed inline.
func fakeCgroup(t *testing.T, usageUsec, memoryCurrent, memoryMax string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// cpu.stat carries several lines and usage_usec is not always first, which
	// is exactly why the parser looks for it by name.
	write("cpu.stat", "nr_periods 0\nusage_usec "+usageUsec+"\nuser_usec 1\nsystem_usec 2\n")
	write("memory.current", memoryCurrent+"\n")
	write("memory.max", memoryMax+"\n")
	return dir
}

func TestTheCPUTotalIsReadByNameAndNotByPosition(t *testing.T) {
	dir := fakeCgroup(t, "2500000", "1048576", "536870912")
	usec, err := readCPUUsec(dir)
	if err != nil {
		t.Fatalf("readCPUUsec: %v", err)
	}
	if usec != 2500000 {
		t.Errorf("usage_usec = %d, want 2500000", usec)
	}
}

// "max" is the kernel's word for no limit. Parsed as a number it would be an
// error, and reported as an error it would make an unlimited machine look
// broken; 0 is the honest answer for "there is no ceiling".
func TestAnUnlimitedMemoryCeilingReadsAsNoCeiling(t *testing.T) {
	dir := fakeCgroup(t, "1", "2048", "max")
	if got := readInt(filepath.Join(dir, "memory.max")); got != 0 {
		t.Errorf("memory.max = %d, want 0 for \"max\"", got)
	}
	if got := readInt(filepath.Join(dir, "memory.current")); got != 2048 {
		t.Errorf("memory.current = %d, want 2048", got)
	}
}

// A file that is not there is not an error to report. A suspended machine has
// no cgroup, and that is the ordinary case rather than a failure.
func TestAMissingCgroupFileIsZeroRatherThanAnError(t *testing.T) {
	if got := readInt(filepath.Join(t.TempDir(), "absent")); got != 0 {
		t.Errorf("a missing file read as %d", got)
	}
	if _, err := readCPUUsec(t.TempDir()); err == nil {
		t.Error("a missing cpu.stat read as a success, so a suspended machine " +
			"would report the cgroup's zero rather than its persisted total")
	}
}

// The whole reason the total is persisted: a counter that goes DOWN makes every
// rate over it negative or enormous, and every alert on it fires on an ordinary
// suspend.
func TestTheCPUTotalSurvivesACgroupBeingDestroyed(t *testing.T) {
	root := t.TempDir()
	m := &Manager{opts: Options{StateRoot: root}}

	// Nothing persisted yet.
	if got := m.carriedCPU("m_1"); got != 0 {
		t.Fatalf("carried = %d before anything was written", got)
	}

	// A machine that ran for 4 seconds, persisted as its cgroup goes away.
	dir := filepath.Join(root, "m_1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, statsFile), []byte(`{"cpu_usec":4000000}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := m.carriedCPU("m_1"); got != 4000000 {
		t.Errorf("carried = %d, want 4000000", got)
	}

	// An unreadable file is zero rather than an error: losing the carried total
	// under-reports, which is bad; failing the whole sample would mean no
	// number at all, which is worse.
	if err := os.WriteFile(filepath.Join(dir, statsFile), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := m.carriedCPU("m_1"); got != 0 {
		t.Errorf("an unreadable stats file gave %d", got)
	}
}
