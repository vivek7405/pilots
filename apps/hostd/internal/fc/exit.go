package fc

import (
	"fmt"
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

// exitPollInterval is the fallback cadence for a kernel without pidfd_open
// (older than 5.3). The reaper's cadence, for the reaper's reason: nothing in
// production reaches this branch, and a timer is never the primitive.
const exitPollInterval = 5 * time.Minute

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
		for processAlive(p.Pid) {
			time.Sleep(exitPollInterval)
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
