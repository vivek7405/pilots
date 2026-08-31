package fc

import (
	"bufio"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Kill must reap, not poll.
//
// After SIGTERM a child becomes a ZOMBIE until it is waited for, and
// kill(pid, 0) on a zombie SUCCEEDS. Polling for its absence therefore burns
// the whole grace period on a process that exited immediately -- silently, on
// the destroy path, for every machine on the host.
func TestKillReapsPromptlyRatherThanPollingAZombie(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	m := &Machine{Cmd: cmd, StateDir: dir}

	start := time.Now()
	if err := m.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > killGrace/2 {
		t.Errorf("Kill took %s; a child that dies on SIGTERM must be reaped, "+
			"not polled for until the %s grace period expires", elapsed, killGrace)
	}
	if syscall.Kill(cmd.Process.Pid, 0) == nil {
		t.Error("the process is still present after Kill")
	}
}

// A child that ignores SIGTERM must still be gone when Kill returns, and Kill
// must not hang waiting for it.
func TestKillEscalatesToSIGKILL(t *testing.T) {
	dir := t.TempDir()

	// trap '' TERM makes the shell ignore SIGTERM outright. It announces
	// itself first, because a signal sent before the trap is installed lands
	// on the default disposition and the process dies obediently -- which
	// looks exactly like the bug this test exists to rule out.
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo ready; sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("the child never installed its trap: %v", err)
	} else if strings.TrimSpace(line) != "ready" {
		t.Fatalf("child said %q, want \"ready\"", line)
	}

	m := &Machine{Cmd: cmd, StateDir: dir}

	start := time.Now()
	if err := m.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < killGrace {
		t.Errorf("Kill returned after %s without giving the process its %s to exit",
			elapsed, killGrace)
	}
	if elapsed > 3*killGrace {
		t.Errorf("Kill took %s; the escalation to SIGKILL did not bound the wait", elapsed)
	}
	if syscall.Kill(cmd.Process.Pid, 0) == nil {
		t.Error("a process that ignored SIGTERM survived Kill")
	}
}
