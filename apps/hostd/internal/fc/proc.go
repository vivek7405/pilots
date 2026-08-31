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
	var errs []error

	if m.Cmd != nil && m.Cmd.Process != nil {
		_ = m.Cmd.Process.Signal(syscall.SIGTERM)

		deadline := time.Now().Add(killGrace)
		for time.Now().Before(deadline) {
			if !processAlive(m.Cmd.Process.Pid) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if processAlive(m.Cmd.Process.Pid) {
			// Kill the whole process group: the jailer's Firecracker child
			// would otherwise survive its parent.
			_ = syscall.Kill(-m.Cmd.Process.Pid, syscall.SIGKILL)
			_ = m.Cmd.Process.Kill()
		}
		// Reap if it is genuinely our child. A reconciled machine is not, and
		// Wait would return ECHILD.
		_, _ = m.Cmd.Process.Wait()
	}

	if m.Slot != nil {
		if err := netns.Teardown(m.Slot); err != nil {
			errs = append(errs, fmt.Errorf("teardown netns: %w", err))
		}
	}

	if m.ChrootDir != "" {
		if err := os.RemoveAll(filepath.Dir(m.ChrootDir)); err != nil {
			errs = append(errs, fmt.Errorf("remove chroot: %w", err))
		}
	}

	if err := ClearBreadcrumbs(m.StateDir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// processAlive reports whether a pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
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
