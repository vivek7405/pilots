package main

import (
	"os/exec"
	"testing"
	"time"
)

// waitUntilDead blocks until pid has exited, without reaping it.
func waitUntilDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := peekDeadChild(); ok && got == pid {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pid %d never showed up as exited", pid)
}

// The reaper must not collect a child os/exec is waiting for.
//
// Wait4(-1) reaps ANY child, which on PID 1 includes the ones os/exec is in
// the middle of waiting for. When the reaper won that race the caller's Wait
// returned ECHILD, exitCodeOf turned it into 127, and a command that had
// actually succeeded was reported as having failed. mke2fs on the volume
// mount path was the one that mattered: a machine came up believing its
// volume was unformatted.
//
// The reaper runs here in the foreground, between the child exiting and its
// owner collecting it, which is precisely the window the race lived in.
func TestTheReaperLeavesOsExecsChildrenAlone(t *testing.T) {
	for i := 0; i < 50; i++ {
		cmd := exec.Command("/bin/sh", "-c", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		release := trackPID(cmd.Process)

		// Wait for the child to actually BE dead before sweeping. Without
		// this the sweep usually runs while it is still alive, finds nothing,
		// and the test passes whatever the reaper would have done -- which is
		// how a first version of this test passed against the very bug it was
		// written to catch.
		waitUntilDead(t, cmd.Process.Pid)

		// Now sweep, in exactly the window the race lived in: the child is a
		// corpse and its owner has not collected it yet.
		reapOrphans()

		err := cmd.Wait()
		release()
		if err != nil {
			t.Fatalf("iteration %d: Wait returned %v; the reaper took the "+
				"status and the caller sees a successful command as failed", i, err)
		}
		if code := exitCodeOf(err); code != 0 {
			t.Fatalf("iteration %d: exit code %d, want 0", i, code)
		}
	}
}

// And it still does its actual job: a child nobody owns is collected, or a
// long-lived guest runs out of pids.
func TestTheReaperCollectsAnOrphan(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Deliberately NOT tracked: this stands in for a process re-parented to
	// PID 1, which no one in this process is waiting for.

	if _, err := cmd.Process.Wait(); err != nil {
		// Either the reaper got it or this did; both mean it is gone.
		_ = err
	}
	reapOrphans()

	if _, owned := ownedPIDs.Load(pid); owned {
		t.Errorf("pid %d is still marked owned", pid)
	}
}
