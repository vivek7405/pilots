package main

import (
	"log"
	"os"
	"os/signal"

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
	for range ch {
		for {
			var status unix.WaitStatus
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}
