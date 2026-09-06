package fc

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExitInfo is what the watcher learned when a machine's Firecracker exited.
type ExitInfo struct {
	Pid    int
	Code   int            // -1 when unknown: signalled, or not our child
	Signal syscall.Signal // 0 when the process was not signalled
	At     time.Time
	// Expected is set when Kill asked for the exit. The machine manager
	// reacts only to an exit nobody asked for.
	Expected bool
	// Adopted is an exit observed through a pidfd rather than a wait: the
	// process outlived the hostd that started it, so its status is unknown.
	Adopted bool
}

// String is the one line written to the machine's lifecycle.log.
func (e ExitInfo) String() string {
	how := fmt.Sprintf("exit code %d", e.Code)
	switch {
	case e.Signal != 0:
		how = fmt.Sprintf("killed by signal %d (%s)", int(e.Signal), e.Signal)
	case e.Adopted:
		how = "status unknown (the process was not this hostd's child)"
	case e.Code == 0:
		how = "exit code 0 (the guest rebooted or powered off)"
	}
	return fmt.Sprintf("[pilots] %s firecracker pid %d exited on its own: %s",
		e.At.UTC().Format(time.RFC3339), e.Pid, how)
}

// The two fallback cadences for a machine whose exit no pidfd can report.
//
// A poll is always the wrong primitive here: every second it is late is a
// second the row says running, the router answers 502 and the idle monitor
// retries a suspend against a corpse, which is the whole of the incident this
// file exists to end. So the slow cadence is reserved for the ONE case that is
// genuinely a property of the host and not a fault: a kernel older than 5.3,
// which has no pidfd_open at all and which no fleet host runs.
//
// Anything else -- EPERM, EMFILE, ENFILE -- is a transient or a limit on a
// host that CAN report exits, so it gets a cadence that keeps the damage to
// seconds. One /proc read per machine per second, on a path nothing should
// ever take.
const (
	exitPollNoPidfd    = 5 * time.Minute
	exitPollUnexpected = time.Second
)

// watchExit starts the goroutine that observes the process's exit, once.
//
// Boot, RestoreInstant and BootFromDisk call it the moment Start returns, so a
// child is reaped the instant it dies and never sits as a zombie. Exited calls
// it too, for a Machine a test or an adoption built by hand.
func (m *Machine) watchExit() {
	m.exitOnce.Do(func() {
		m.exited = make(chan struct{})
		if m.Cmd == nil || m.Cmd.Process == nil {
			m.markExited(ExitInfo{Code: -1})
			return
		}
		go m.waitForExit(m.Cmd.Process)
	})
}

func (m *Machine) waitForExit(p *os.Process) {
	// Our child: Wait reaps it and reports how it went.
	if st, err := p.Wait(); err == nil {
		info := ExitInfo{Pid: p.Pid, Code: st.ExitCode()}
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			info.Signal = ws.Signal()
		}
		m.markExited(info)
		return
	}
	// Not our child (ECHILD): a re-adopted machine. A pidfd reports its exit
	// without being its parent; the process's own parent reaps it.
	info := ExitInfo{Pid: p.Pid, Code: -1, Adopted: true}
	switch err := waitPidfd(p.Pid); err {
	case nil, unix.ESRCH:
		m.markExited(info)
	default:
		every := exitPollFor(err)
		if every != exitPollNoPidfd {
			slog.Warn("could not watch a machine's exit through a pidfd; falling "+
				"back to polling its pid", "machine", m.ID, "pid", p.Pid,
				"every", every, "err", err)
		}
		for processAlive(p.Pid) {
			time.Sleep(every)
		}
		m.markExited(info)
	}
}

// waitPidfd blocks until the pid exits. ESRCH means it already has; ENOSYS
// means the kernel has no pidfd_open and the caller must poll.
func waitPidfd(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != unix.EINTR {
			return err
		}
	}
}

// markExited records the exit once and wakes every waiter.
func (m *Machine) markExited(info ExitInfo) {
	m.exitMu.Lock()
	defer m.exitMu.Unlock()
	if m.exitInfo != nil {
		return
	}
	if info.At.IsZero() {
		info.At = time.Now()
	}
	info.Expected = m.expectExit.Load()
	m.exitInfo = &info
	close(m.exited)
}

// Exited is closed when the machine's Firecracker has exited, for any reason.
func (m *Machine) Exited() <-chan struct{} {
	m.watchExit()
	return m.exited
}

// Exit is what happened, valid once Exited is closed.
func (m *Machine) Exit() ExitInfo {
	m.exitMu.Lock()
	defer m.exitMu.Unlock()
	if m.exitInfo == nil {
		return ExitInfo{}
	}
	return *m.exitInfo
}

// exitPollFor picks the cadence to fall back to when pidfd_open failed.
//
// Only a kernel with no pidfd_open earns the slow one. Every other failure --
// EPERM, EMFILE, ENFILE -- is on a host that CAN report exits, so being
// minutes late there would be a choice rather than a limit, and those minutes
// are exactly the window this whole file exists to close.
func exitPollFor(err error) time.Duration {
	if err == unix.ENOSYS {
		return exitPollNoPidfd
	}
	return exitPollUnexpected
}
