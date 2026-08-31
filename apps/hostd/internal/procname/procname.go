// Package procname renames the running process.
package procname

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Set renames the process as tools like pkill and ps see it.
//
// hostd re-executes itself to serve a machine's disk and memory, so without
// this those handlers are called "hostd" too -- and a pkill aimed at the
// daemon, from an operator or a supervisor, takes every machine on the host
// with it. A guest whose block or fault server has gone away blocks in an
// uninterruptible wait that no signal clears.
//
// Three details make this work, and it silently does nothing without any of
// them:
//
//   - PR_SET_NAME renames the CALLING THREAD, while /proc/<pid>/comm -- what
//     pkill matches -- reports the MAIN thread. So this locks to the current
//     thread, and must be called from main before anything can migrate the
//     goroutine off it.
//   - The name must be NUL-terminated and at most 16 bytes including that
//     terminator, or the kernel truncates it into something pkill will not
//     match.
//   - The pointer is converted inside the syscall.Syscall argument list, which
//     is the one place the unsafe.Pointer rules allow a pointer to become a
//     uintptr. Building it anywhere else leaves the buffer unreferenced --
//     a uintptr is not a reference the collector follows -- so it may be
//     reclaimed before the kernel reads it.
func Set(name string) error {
	const maxLen = 15 // TASK_COMM_LEN is 16, including the terminator

	if len(name) > maxLen {
		return fmt.Errorf("procname: %q is longer than %d bytes", name, maxLen)
	}
	runtime.LockOSThread()

	buf := append([]byte(name), 0)
	if _, _, errno := syscall.Syscall(unix.SYS_PRCTL,
		unix.PR_SET_NAME, uintptr(unsafe.Pointer(&buf[0])), 0); errno != 0 {
		return fmt.Errorf("procname: set %q: %w", name, errno)
	}
	return nil
}
