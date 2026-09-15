package main

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// becomeIOFlusher marks the NBD handler as an I/O flusher, re-executing it if
// that is what it takes for every thread to carry the mark, and returns only
// once they all do (or once it is clear the kernel will not allow it).
//
// Without it the handler deadlocks against the kernel on any host with a low
// dirty-page limit. A guest's writes land in the page cache of /dev/nbdN, and
// they can only be written back through this process. The handler writes each
// one into the copy-on-write file through a shared mapping, which dirties a
// page, and the kernel throttles a task that dirties pages while the host is
// over its limit -- mostly with the device's own pages, which are waiting on
// this very task. Nothing moves again. The guest shows "jbd2/vda blocked for
// more than 120 seconds" and the build or deploy inside it hangs for good.
// Measured on a laptop with vm.dirty_bytes=256M: a builder froze mid npm
// install, and a 2 GiB dd inside a sandbox reproduced it in under a minute.
// scripts/host-bootstrap.sh raising dirty_ratio only makes this rarer, and
// makes the outcome depend on a sysctl nothing checks.
//
// PR_SET_IO_FLUSHER is the kernel's answer for exactly this process: a
// userspace block server. It sets PF_LOCAL_THROTTLE, which throttles the task
// on its own backing device's dirty pages rather than the host's, and
// PF_MEMALLOC_NOIO, so reclaim inside it never waits on I/O it has to serve.
//
// The flag is per THREAD, and the Go runtime has several before main runs, so
// setting it here would leave most goroutines running on threads without it.
// It survives execve and a new thread inherits it from the one that cloned it,
// so the handler sets it on a locked thread and re-executes itself: every
// thread of the new runtime then descends from a flagged one. Same pid, same
// arguments, same inherited descriptors, so hostd's view of the child and its
// ready pipe are untouched.
func becomeIOFlusher() {
	if ioFlusher() {
		return
	}
	runtime.LockOSThread()
	if err := setIOFlusher(); err != nil {
		// Not fatal. A kernel older than 5.6 or a handler without
		// CAP_SYS_RESOURCE serves as before, and only a host short of dirty
		// headroom is exposed. Said out loud, because the hang it leads to
		// logs nothing at all.
		runtime.UnlockOSThread()
		slog.Warn("could not mark the nbd handler as an I/O flusher; a host with a "+
			"low dirty-page limit can deadlock this machine's disk", "err", err)
		return
	}
	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, os.Args, os.Environ())
	}
	// Only reached when the exec failed. This thread is flagged and the others
	// are not, which is no worse than never trying.
	slog.Warn("could not re-execute the nbd handler as an I/O flusher; a host with a "+
		"low dirty-page limit can deadlock this machine's disk", "err", err)
}

// ioFlusher reports whether the calling thread carries PR_SET_IO_FLUSHER.
// After the re-exec in becomeIOFlusher that is every thread.
func ioFlusher() bool {
	v, _, errno := syscall.Syscall(unix.SYS_PRCTL, unix.PR_GET_IO_FLUSHER, 0, 0)
	return errno == 0 && v == 1
}

func setIOFlusher() error {
	if _, _, errno := syscall.Syscall(unix.SYS_PRCTL, unix.PR_SET_IO_FLUSHER, 1, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_IO_FLUSHER): %w", errno)
	}
	return nil
}
