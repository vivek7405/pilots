// Package procname renames the running process.
package procname

import (
	"fmt"
	"runtime"

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
// Two details make this work, and it silently does nothing without either.
// PR_SET_NAME renames the CALLING THREAD, while /proc/<pid>/comm -- which is
// what pkill matches -- reports the MAIN thread; so this locks to the current
// thread and must be called from main before anything else can migrate the
// goroutine off it. And the name must be NUL-terminated and at most 16 bytes
// including that NUL, or the kernel truncates it somewhere unhelpful.
func Set(name string) error {
	const maxLen = 15 // TASK_COMM_LEN is 16, including the terminator

	if len(name) > maxLen {
		return fmt.Errorf("procname: %q is longer than %d bytes", name, maxLen)
	}
	runtime.LockOSThread()

	if err := unix.Prctl(unix.PR_SET_NAME, uintptrOf(name), 0, 0, 0); err != nil {
		return fmt.Errorf("procname: set %q: %w", name, err)
	}
	return nil
}
