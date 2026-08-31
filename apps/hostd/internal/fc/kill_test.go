package fc

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
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

// cpuTimeMS reads this process's own CPU time, user plus system.
func cpuTimeMS(t *testing.T) int64 {
	t.Helper()

	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatalf("read /proc/self/stat: %v", err)
	}
	// The comm field may contain spaces, so fields are counted from after it.
	stat := string(raw)
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		t.Fatalf("parse utime: %v", err)
	}
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		t.Fatalf("parse stime: %v", err)
	}
	const ticksPerSecond = 100 // USER_HZ
	return (utime + stime) * 1000 / ticksPerSecond
}

// Kill must WAIT out the grace period, not spin through it.
//
// For an ADOPTED machine the process is not our child, so Wait returns ECHILD
// at once and closes the channel it signals on. A closed channel is always
// ready, so leaving it in the select turns the wait into a busy loop issuing a
// kill(2) per iteration -- a full core for two seconds, per machine, on the
// path a restarted daemon takes to reap what it re-adopted.
func TestKillWaitsRatherThanSpinning(t *testing.T) {
	// A process that is alive, ignores SIGTERM, and is NOT our child: exactly
	// the shape Kill sees after re-adopting a machine. setsid detaches it, so
	// Wait on it fails the way it does for a re-adopted Firecracker.
	launch := exec.Command("sh", "-c",
		"setsid sh -c \"trap '' TERM; echo ready; sleep 30\" & wait")
	stdout, err := launch.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = launch.Process.Kill() }()

	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("the detached process never announced itself: %v", err)
	}

	// Find it: a session leader running sleep, not parented to this test.
	pid := findDetachedSleep(t)
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	m := &Machine{Cmd: &exec.Cmd{Process: proc}, StateDir: t.TempDir()}

	before := cpuTimeMS(t)
	start := time.Now()
	_ = m.Kill()
	elapsed := time.Since(start)
	spent := cpuTimeMS(t) - before

	if elapsed < killGrace {
		t.Fatalf("Kill returned after %s without waiting out the %s grace period; "+
			"this test is no longer exercising the wait", elapsed, killGrace)
	}
	// Waiting costs nothing; spinning costs a core for the whole period.
	if spent > killGrace.Milliseconds()/4 {
		t.Errorf("Kill burned %dms of CPU waiting %s; it is spinning, not waiting",
			spent, elapsed)
	}
}

// findDetachedSleep locates the setsid'ed sleep started above.
func findDetachedSleep(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "sleep" {
			continue
		}
		// Session leader: setsid made it one, so this is ours rather than some
		// unrelated sleep on the machine.
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		fields := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
		if len(fields) > 3 && fields[3] == e.Name() {
			return pid
		}
	}
	t.Skip("could not find the detached process; the shell may not support setsid here")
	return 0
}
