package machines

import (
	"context"
	"github.com/pilotsrun/pilots/hostd/internal/state"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMachineIDFromCmdline(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			"jailer-style invocation",
			[]string{"/firecracker", "--id", "m-abc123", "--api-sock", "/run/fc.sock"},
			"m-abc123",
		},
		{
			"id at the end",
			[]string{"/firecracker", "--api-sock", "/run/fc.sock", "--id", "m-xyz"},
			"m-xyz",
		},
		{
			"no id at all",
			[]string{"/firecracker", "--api-sock", "/run/fc.sock"},
			"",
		},
		{
			// A trailing --id with nothing after it must not read past the end.
			"dangling flag",
			[]string{"/firecracker", "--id"},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			var raw []byte
			for _, a := range tc.args {
				raw = append(raw, []byte(a)...)
				raw = append(raw, 0)
			}
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := machineIDFromCmdline(path); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMachineIDFromMissingCmdlineIsEmpty(t *testing.T) {
	if got := machineIDFromCmdline("/proc/nonexistent/cmdline"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// The reaper must not see this test binary as a Firecracker to kill.
func TestFirecrackerProcessesIgnoresOtherProcesses(t *testing.T) {
	self := os.Getpid()
	for _, p := range firecrackerProcesses() {
		if p.pid == self {
			t.Fatal("the reaper identified the test binary as a firecracker")
		}
	}
}

// The sweep removes only what no row names and what has been on disk long
// enough that nothing can still be about to name it.
func TestOrphanBuildSweepKeepsReferencedAndYoungDirectories(t *testing.T) {
	dirs := []buildDirInfo{
		{id: "old-orphan", age: 3 * time.Hour},
		{id: "young-orphan", age: 10 * time.Minute},
		{id: "old-referenced", age: 3 * time.Hour},
	}
	got := selectOrphanBuilds(dirs, map[string]bool{"old-referenced": true})
	if len(got) != 1 || got[0] != "old-orphan" {
		t.Fatalf("selectOrphanBuilds = %v, want only the old orphan", got)
	}
}

// Every id a row can name is protected: the machine's disk and memory, its
// template pair, the image it booted and its checkpoints.
func TestReferencedBuildsNamesEverythingARowCan(t *testing.T) {
	ctx := context.Background()
	m, st := storeManager(t)
	if err := st.PutMachine(ctx, &state.Machine{ID: "m-1", HostID: "host-a", State: StateRunning,
		RootfsBuildID: "disk", MemBuildID: "mem", TemplateMemBuildID: "tmem", TemplateRootfsBuildID: "tdisk", ImageRef: "image"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCheckpoint(ctx, &state.Checkpoint{ID: "ck-1", MachineID: "m-1", RootfsBuildID: "ckdisk", MemBuildID: "ckmem"}); err != nil {
		t.Fatal(err)
	}
	ref, ok := m.referencedBuilds(ctx)
	if !ok {
		t.Fatal("referencedBuilds could not read a store it just wrote")
	}
	for _, want := range []string{"disk", "mem", "tmem", "tdisk", "image", "ckdisk", "ckmem"} {
		if !ref[want] {
			t.Errorf("%q is not protected: %v", want, ref)
		}
	}
}
