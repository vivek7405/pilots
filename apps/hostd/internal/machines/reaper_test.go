package machines

import (
	"os"
	"path/filepath"
	"testing"
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
