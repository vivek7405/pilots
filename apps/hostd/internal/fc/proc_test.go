package fc

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A zombie is not alive.
//
// kill(pid, 0) succeeds on a zombie, and a zombie is exactly what a
// Firecracker that died under hostd is until something waits for it. With the
// old check, LiveProcess called a corpse live and reconcile re-adopted it on
// every hostd start, so a machine whose guest was long gone kept its row at
// running for as long as the host stayed up.
func TestProcessAliveIsFalseForAZombie(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Deliberately NOT waited for yet: that is what makes it a zombie.
	defer func() { _, _ = cmd.Process.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && procState(t, pid) != "Z" {
		time.Sleep(5 * time.Millisecond)
	}
	if got := procState(t, pid); got != "Z" {
		t.Fatalf("the child's state is %q, want Z; this test is not exercising a zombie", got)
	}

	if syscall.Kill(pid, 0) != nil {
		t.Fatal("kill(pid, 0) already fails on this zombie; the test cannot show the difference")
	}
	if processAlive(pid) {
		t.Error("processAlive says a zombie is alive")
	}
	if LiveProcess(pid) != 0 {
		t.Error("LiveProcess returns a zombie's pid, so reconcile would re-adopt a corpse")
	}
}

// procState reads the state field of /proc/<pid>/stat, or "" when it is gone.
func procState(t *testing.T, pid int) string {
	t.Helper()

	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	stat := string(raw)
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
