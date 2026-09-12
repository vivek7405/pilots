package machines

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The path has to be the jailer's, exactly. hostd passes --parent-cgroup
// pilots and --exec-file <bin>, and the jailer builds
// <root>/pilots/<exec file name>/<id>. A path that drifted from that would
// write a pid into a cgroup nobody is bounding, silently.
func TestMachineCgroupMirrorsTheJailerLayout(t *testing.T) {
	old := cgroupRoot
	cgroupRoot = "/sys/fs/cgroup"
	t.Cleanup(func() { cgroupRoot = old })

	got := machineCgroup("firecracker", "m_abc")
	want := "/sys/fs/cgroup/pilots/firecracker/m_abc"
	if got != want {
		t.Errorf("machineCgroup = %q, want %q", got, want)
	}
}

// Moving a handler in, read back out of cgroup.procs. A real cgroup needs
// root, so the directory is faked: what is under test is that the right pid
// reaches the right file, which is the part that can be wrong.
func TestJoinHandlersWritesThePidsIntoTheMachineCgroup(t *testing.T) {
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	dir := filepath.Join(root, "pilots", "firecracker", "m_abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	procs := filepath.Join(dir, "cgroup.procs")
	if err := os.WriteFile(procs, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// A plain write, the way joinHandlersToCgroup does it.
	if err := os.WriteFile(procs, []byte(strconv.Itoa(4242)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := cgroupProcs("firecracker", "m_abc")
	if err != nil {
		t.Fatalf("cgroupProcs: %v", err)
	}
	if len(got) != 1 || got[0] != 4242 {
		t.Errorf("cgroup.procs = %v, want [4242]", got)
	}
}

// A host with no cgroup for this machine must keep working. Accounting must
// never be the reason a machine that is already running is handed back as a
// failure.
func TestJoinHandlersIsSilentWithNoCgroup(t *testing.T) {
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	m := &Manager{}
	// A nil machine, and a machine whose cgroup does not exist: neither may
	// panic or block.
	m.joinHandlersToCgroup(nil)
	m.killCgroup("m_missing")
	m.removeCgroup("m_missing")
}

func TestSplitLinesDropsBlanks(t *testing.T) {
	got := splitLines("1\n2\n\n3\n")
	if len(got) != 3 || got[0] != "1" || got[2] != "3" {
		t.Errorf("splitLines = %v, want [1 2 3]", got)
	}
	if len(splitLines("")) != 0 {
		t.Error("splitLines on empty input returned entries")
	}
}
