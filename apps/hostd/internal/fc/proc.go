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
		pid := m.Cmd.Process.Pid
		_ = m.Cmd.Process.Signal(syscall.SIGTERM)

		// Reaping, not polling. After SIGTERM the process becomes a ZOMBIE
		// until it is waited for, and kill(pid, 0) on a zombie SUCCEEDS -- so
		// polling for its absence burns the whole grace period on a process
		// that already exited. Nothing fails; every destroy on the host just
		// takes killGrace longer than it should.
		//
		// An adopted machine is not our child, so Wait returns ECHILD at once
		// and the poll below is what actually observes its exit.
		reaped := make(chan struct{})
		go func() { _, _ = m.Cmd.Process.Wait(); close(reaped) }()

		deadline := time.After(killGrace)
		poll := time.NewTicker(20 * time.Millisecond)
		defer poll.Stop()

	wait:
		for {
			select {
			case <-reaped:
				if !processAlive(pid) {
					break wait
				}
			case <-poll.C:
				if !processAlive(pid) {
					break wait
				}
			case <-deadline:
				// Kill the whole process group: the jailer's Firecracker child
				// would otherwise survive its parent.
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = m.Cmd.Process.Kill()
				<-reaped
				break wait
			}
		}
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
