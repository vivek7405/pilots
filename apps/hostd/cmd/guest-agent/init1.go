package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The agent as PID 1.
//
// Most real Dockerfiles are built on images that contain no init system at
// all -- node:24-alpine, python:slim, distroless. Docker never needed one,
// because a container's kernel is the host's and its filesystems are set up
// from outside. A microVM has neither: the kernel boots /sbin/init, and if
// that is a Node process then nothing has mounted /proc, the root filesystem
// is still read-only from the ro boot argument, and every orphaned process
// becomes an unreaped zombie.
//
// So the build path points /sbin/init at this binary for any image without
// systemd, and the agent does the small amount of work a container runtime
// used to do. An image that DOES carry systemd is left alone: systemd is PID 1
// there and the agent runs as an ordinary unit.
//
// This deliberately does NOT start the application. A create is a resume, and
// the environment a service runs with is delivered after the resume -- so the
// application is exec'd by the agent once it has its environment, never by
// init. See ARCHITECTURE.md on the golden template stopping short of the app.

// isPID1 reports whether this process is the guest's init.
func isPID1() bool { return os.Getpid() == 1 }

// mountpoint is one of the pseudo-filesystems a Linux userspace assumes.
type mountpoint struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
	mode                   os.FileMode
}

// The set is the minimum a normal userspace breaks without:
//   - /proc, or nothing can read its own pid, and the agent cannot reap.
//   - /sys, for anything that reads device or network state.
//   - /dev as devtmpfs, so /dev/vdb exists to mount a volume from. Without
//     it the volume device is simply not there and the mount fails with a
//     message about a missing file.
//   - /dev/pts, for the terminal endpoint.
//   - /run and /tmp, which almost everything writes to.
var initMounts = []mountpoint{
	{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV, "", 0o555},
	{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV, "", 0o555},
	{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755", 0o755},
	{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "gid=5,mode=620", 0o755},
	{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755", 0o755},
	{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777", 0o1777},
}

// runAsInit does what PID 1 owes the rest of the guest, then returns so the
// agent can serve.
//
// Every step is best effort and logs rather than exiting. PID 1 exiting is a
// kernel panic, so a machine that fails to mount /tmp must still come up far
// enough to be reachable and told what went wrong -- an unreachable machine
// cannot be debugged at all.
func runAsInit() {
	// The kernel mounted the root read-only, because the boot arguments say
	// `ro`. systemd would remount it; here there is nobody else to do it, and
	// an application that cannot write anywhere is not a working machine.
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT, ""); err != nil {
		log.Printf("guest-agent: remount / read-write: %v", err)
	}

	for _, m := range initMounts {
		if err := os.MkdirAll(m.target, m.mode); err != nil {
			log.Printf("guest-agent: mkdir %s: %v", m.target, err)
			continue
		}
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil &&
			err != unix.EBUSY {
			// EBUSY means it is already mounted, which is success.
			log.Printf("guest-agent: mount %s on %s: %v", m.fstype, m.target, err)
		}
	}

	go reapChildren()
}

// reapChildren collects orphans.
//
// Every process whose parent dies is re-parented to PID 1, and a PID 1 that
// never waits leaves each one as a zombie holding a pid slot. A long-lived
// machine running an application that forks eventually cannot create a process
// at all, and the error it reports is about resources rather than about this.
func reapChildren() {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, unix.SIGCHLD)

	// A tick as well as the signal. Standard signals do not queue, so two
	// children exiting close together can raise ONE SIGCHLD -- and a sweep
	// that stops early because the first corpse it found belongs to os/exec
	// would otherwise leave the second lying there until something else
	// happened to die.
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ch:
		case <-tick.C:
		}
		reapOrphans()
	}
}

// reapOrphans collects the dead this process is responsible for, and only
// those.
//
// Wait4(-1) reaps ANY child, which on PID 1 includes the ones os/exec is in
// the middle of waiting for. When the reaper won that race the caller's Wait
// returned ECHILD, exitCodeOf turned it into 127, and a command that had
// actually succeeded was reported as having failed -- mke2fs on the volume
// mount path being the one that mattered, since a machine then came up
// believing its volume was unformatted.
//
// So each corpse is identified BEFORE it is collected. WNOWAIT reports who
// died without consuming the status, leaving it for os/exec if os/exec owns
// it.
func reapOrphans() {
	for {
		pid, ok := peekDeadChild()
		if !ok {
			return
		}
		if _, owned := ownedPIDs.Load(pid); owned {
			// Leave it. Its owner is about to collect it, and the next tick
			// picks up anything queued behind it.
			return
		}
		var status unix.WaitStatus
		if _, err := unix.Wait4(pid, &status, unix.WNOHANG, nil); err != nil {
			return
		}
	}
}

// peekDeadChild names a child that has exited without reaping it.
func peekDeadChild() (int, bool) {
	var info siginfoChld
	err := unix.Waitid(unix.P_ALL, 0, (*unix.Siginfo)(unsafe.Pointer(&info)),
		unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
	if err != nil || info.Pid == 0 {
		return 0, false
	}
	return int(info.Pid), true
}

// siginfoChld is siginfo_t as the kernel fills it in for SIGCHLD.
//
// x/sys/unix models the union as opaque padding, so there is no exported way
// to read si_pid out of it. Same size and layout, with the fields this needs
// named.
type siginfoChld struct {
	Signo, Errno, Code int32
	_                  int32
	Pid, Uid           int32
	Status             int32
	_                  [100]byte
}

// ownedPIDs are the children os/exec is waiting for. The reaper must not
// collect these; whoever started them is going to.
var ownedPIDs sync.Map

// trackPID marks a child as owned until the returned function is called.
func trackPID(p *os.Process) func() {
	if p == nil {
		return func() {}
	}
	ownedPIDs.Store(p.Pid, struct{}{})
	return func() { ownedPIDs.Delete(p.Pid) }
}

// runTracked is Run for a process the reaper must keep its hands off.
func runTracked(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	defer trackPID(cmd.Process)()
	return cmd.Wait()
}

// combinedOutputTracked is CombinedOutput with the same protection.
func combinedOutputTracked(cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := runTracked(cmd)
	return buf.Bytes(), err
}
