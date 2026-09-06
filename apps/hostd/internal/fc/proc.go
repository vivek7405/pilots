package fc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// killGrace is the SHARED deadline for every child of a machine to exit after
// SIGTERM. Shared, not per-child: three children each given their own two
// seconds turns a teardown into six, and destroy is on the hot path.
const killGrace = 2 * time.Second

// Kill tears a machine down completely and leaves nothing behind.
//
// Order matters, and each step here corresponds to a way the predecessor
// leaked:
//
//  1. SIGTERM every child, then ONE shared wait, then SIGKILL the stragglers.
//     Signalling without waiting orphans children to PID 1 -- that is how a
//     host once accumulated seven zombie Firecrackers and then failed every
//     subsequent operation with "too many open connections".
//  2. Tear down the namespace only after the processes holding it are gone,
//     retrying past EBUSY.
//  3. Remove the breadcrumbs LAST. While fc.pid exists, reconcile treats the
//     machine as live and will happily resurrect a machine that was destroyed.
func (m *Machine) Kill() error {
	if m.Cmd != nil && m.Cmd.Process != nil {
		pid := m.Cmd.Process.Pid
		// Before the signal, never after: the watcher reads this flag the
		// moment the process is gone, and an exit it saw first is an exit the
		// machine manager would bring the machine back from.
		m.expectExit.Store(true)
		exited := m.Exited()
		_ = m.Cmd.Process.Signal(syscall.SIGTERM)

		// Reaping, not polling. The watcher started at Start reaps our own
		// child and holds a pidfd on an adopted one, so this select wakes the
		// instant the process is gone -- for a zombie too, which kill(pid, 0)
		// would have reported alive for the whole grace period.
		select {
		case <-exited:
		case <-time.After(killGrace):
			// Kill the whole process group: the jailer's Firecracker child
			// would otherwise survive its parent.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = m.Cmd.Process.Kill()
			select {
			case <-exited:
			case <-time.After(killGrace):
				// Only the pidfd-less fallback poller can get here; SIGKILL is
				// not refusable, so the process is gone and the poller notices
				// on its next tick.
			}
		}
	}
	return m.Cleanup()
}

// Cleanup removes everything a machine holds on the host once its Firecracker
// is gone: the handlers, the namespace, the volume bind, the chroot and the
// breadcrumbs, in that order. Kill calls it after the process; the machine
// manager calls it directly for a process that exited on its own.
func (m *Machine) Cleanup() error {
	var errs []error

	// After Firecracker, never before: the handlers serve its disk and its
	// memory, and taking either away from a live guest leaves it in an
	// uninterruptible wait that no signal clears.
	errs = append(errs, m.stopHandlers()...)

	if m.Slot != nil {
		if err := netns.Teardown(m.Slot); err != nil {
			errs = append(errs, fmt.Errorf("teardown netns: %w", err))
		}
	}

	if m.ChrootDir != "" {
		// Before the removal, always. RemoveAll cannot unlink a mountpoint, so
		// a volume still bound into the jail fails the teardown -- and keeps
		// the volume's file open, so juicefs refuses to unmount and the volume
		// stays pinned to a host that no longer runs its machine.
		if err := unstageVolume(m.ChrootDir); err != nil {
			errs = append(errs, err)
		}
		if err := os.RemoveAll(filepath.Dir(m.ChrootDir)); err != nil {
			errs = append(errs, fmt.Errorf("remove chroot: %w", err))
		}
	}

	if err := ClearBreadcrumbs(m.StateDir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// processAlive reports whether a pid names a process that is still running.
//
// NOT kill(pid, 0): that succeeds on a zombie, and a zombie is exactly what a
// Firecracker that died under hostd is until something waits for it. The state
// field of /proc/<pid>/stat is the first field after the closing parenthesis
// of comm, which may itself contain spaces and parentheses.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	stat := string(raw)
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

// isFirecracker checks that a pid is actually a Firecracker process.
//
// The pid alone is not enough: pids are recycled, and a stale breadcrumb can
// name a pid the kernel has since handed to something unrelated. Adopting that
// would mean hostd sends lifecycle signals to an innocent process.
func isFirecracker(pid int) bool {
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(comm))
	return name == "firecracker" || name == "jailer"
}

// LiveProcess returns the pid if it is a live Firecracker, else 0.
//
// Both checks are needed and in this order: the comm check rules out pid
// recycling, and the liveness re-check catches a process that died between the
// two reads.
func LiveProcess(pid int) int {
	if pid <= 0 || !isFirecracker(pid) || !processAlive(pid) {
		return 0
	}
	return pid
}
