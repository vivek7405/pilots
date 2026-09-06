package fc

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
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

// An exit hostd did not ask for must close Exited and say how it happened.
//
// Without the watcher the process stays a ZOMBIE under hostd: kill(pid, 0)
// keeps succeeding, every loop that keys on "is it alive" keeps saying yes,
// and nothing on the host ever learns the guest is gone. That is the whole
// incident this file's watcher exists to prevent.
func TestExitedFiresWhenTheChildDies(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	m := &Machine{Cmd: cmd, StateDir: t.TempDir()}
	exited := m.Exited()

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the exit was never observed")
	}

	info := m.Exit()
	if info.Signal != syscall.SIGKILL {
		t.Errorf("Exit().Signal is %v, want SIGKILL", info.Signal)
	}
	if info.Expected {
		t.Error("an exit nobody asked for is marked Expected")
	}
	if info.Pid != pid {
		t.Errorf("Exit().Pid is %d, want %d", info.Pid, pid)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Error("the process is still a zombie; the watcher did not reap it")
	}
}

// Kill must claim the exit it causes.
//
// The machine manager restarts a machine whose Firecracker exited without
// being asked. If Kill did not set the flag, every destroy and every suspend
// would look like a crash and the host would bring back what it just took
// down.
func TestKillMarksItsExitExpected(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	m := &Machine{Cmd: cmd, StateDir: t.TempDir()}
	if err := m.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !m.Exit().Expected {
		t.Error("Kill's own exit is not marked Expected")
	}
}

// An ADOPTED machine's exit must be observed too.
//
// Its process is not this hostd's child, so Wait returns ECHILD at once and
// reports nothing. A pidfd tells a non-parent when a process exits, which is
// what keeps a machine re-adopted across a hostd restart from going back to
// being invisible when it dies.
func TestAdoptedMachineExitIsObservedViaPidfd(t *testing.T) {
	pid := startDetachedSleep(t)
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	m := &Machine{Cmd: &exec.Cmd{Process: proc}, StateDir: t.TempDir()}
	exited := m.Exited()

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("an adopted process's exit was not observed; the pidfd wait did not fire")
	}
	if !m.Exit().Adopted {
		t.Error("the exit is not marked Adopted, so its status was read as a child's")
	}
}

// startDetachedSleep launches a sleep that is NOT this process's child and
// returns its pid.
//
// The pid comes from the process itself through a file rather than from a scan
// of /proc: a scan picks up any session-leading sleep on the machine,
// including another test binary's, and then signals it.
func startDetachedSleep(t *testing.T) int {
	t.Helper()

	pidFile := filepath.Join(t.TempDir(), "pid")
	launch := exec.Command("sh", "-c",
		"setsid sh -c 'echo $$ > "+pidFile+"; exec sleep 60' & wait")
	if err := launch.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = launch.Process.Kill(); _, _ = launch.Process.Wait() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(raw))); cerr == nil && pid > 0 {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				// exec has to have happened, or the pid still names the shell.
				for time.Now().Before(deadline) {
					comm, cerr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
					if cerr == nil && strings.TrimSpace(string(comm)) == "sleep" {
						return pid
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("the detached shell never exec'd into sleep")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the detached process never reported its pid")
	return 0
}
