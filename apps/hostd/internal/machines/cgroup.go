package machines

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/vivek7405/pilots/hostd/internal/fc"
)

// Putting a machine's handlers in the machine's own cgroup.
//
// The jailer creates a cgroup per machine and puts Firecracker in it, so a
// guest's memory and its fork bombs are bounded. Its handlers were not in it:
// they are hostd's children, spawned before the jailer runs, so they sat in
// hostd's own slice. The consequences are two, and both are real rather than
// theoretical.
//
// A handler's page cache and its chunk cache are memory the guest caused to be
// allocated. Charged to hostd's slice, one machine reading its whole disk
// pushes the DAEMON toward its MemoryMax, and what gets killed is hostd rather
// than the machine responsible. And a destroy that killed only Firecracker
// left a handler alive charged to nobody, which is how a "destroyed" machine
// keeps costing memory.
//
// The move happens AFTER Firecracker is up, by writing each handler's pid into
// the cgroup the jailer just made, rather than by creating that cgroup here
// first. That ordering is the whole design: the handlers must be listening
// before Firecracker restores against them, so anything that had to exist
// before them would have to be built by hostd and then adopted by the jailer,
// and a jailer that found its cgroup already populated is a boot path nobody
// wants to debug at three in the morning. Writing a pid into an existing
// cgroup is one line, reversible, and cannot fail the boot.

// cgroupRoot is where cgroup v2 is mounted. A variable for the tests.
var cgroupRoot = "/sys/fs/cgroup"

// machineCgroup is the directory the jailer makes for one machine.
//
// The layout is the jailer's: <root>/<parent-cgroup>/<exec file name>/<id>.
// Built from the same three values hostd passes it (see fc.jailerArgs), so the
// two cannot drift without this failing loudly rather than silently writing
// into the wrong slice.
func machineCgroup(execFileName, machineID string) string {
	return filepath.Join(cgroupRoot, "pilots", execFileName, machineID)
}

// joinHandlersToCgroup moves a machine's handler processes into its cgroup.
//
// Best effort by design. Every failure here is logged and swallowed: the
// machine is already running and serving, and refusing to hand it over because
// an accounting move failed would trade a real outage for a bookkeeping
// problem. A host without cgroup v2 at this path simply keeps today's
// behaviour.
func (m *Manager) joinHandlersToCgroup(fcm *fc.Machine) {
	if fcm == nil {
		return
	}
	dir := machineCgroup(filepath.Base(m.opts.FCConfig.FirecrackerBin), fcm.ID)
	procs := filepath.Join(dir, "cgroup.procs")
	if _, err := os.Stat(dir); err != nil {
		// No cgroup for this machine: a host with no jailer cgroup support, or
		// a boot path that did not make one.
		return
	}
	for _, pid := range []int{fcm.NBD.PID(), fcm.Uffd.PID()} {
		if pid <= 0 {
			continue
		}
		if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)), 0o644); err != nil {
			slog.Warn("could not move a handler into its machine's cgroup; its "+
				"memory stays charged to hostd", "machine", fcm.ID, "pid", pid, "err", err)
		}
	}
}

// killCgroup kills everything left in a machine's cgroup.
//
// Written after Firecracker has been killed, as the sweep that catches what a
// per-pid teardown misses: a handler wedged in an uninterruptible wait, a
// process a guest escape left behind, anything the jailer's own children
// spawned. cgroup.kill is a single write the kernel applies to every member at
// once, which is the one teardown that cannot race a fork.
func (m *Manager) killCgroup(machineID string) {
	dir := machineCgroup(filepath.Base(m.opts.FCConfig.FirecrackerBin), machineID)
	path := filepath.Join(dir, "cgroup.kill")
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil && !os.IsNotExist(err) {
		slog.Debug("could not kill a machine's cgroup", "machine", machineID, "err", err)
	}
}

// removeCgroup removes a machine's cgroup directory once it is empty.
//
// An empty cgroup directory is not free: the kernel keeps a structure per
// cgroup, and a host that created one per machine and removed none would
// accumulate them for as long as it stays up. Called by the reaper, which is
// the one thing that knows a machine is gone for good.
func (m *Manager) removeCgroup(machineID string) {
	dir := machineCgroup(filepath.Base(m.opts.FCConfig.FirecrackerBin), machineID)
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		// ENOTEMPTY means something is still in it, which the kill above
		// should have handled: worth a line, because it is a leak.
		slog.Debug("could not remove a machine's cgroup", "machine", machineID, "err", err)
	}
}

// cgroupProcs reads the pids in a machine's cgroup. For the tests and for a
// diagnostic; nothing on the request path calls it.
func cgroupProcs(execFileName, machineID string) ([]int, error) {
	raw, err := os.ReadFile(filepath.Join(machineCgroup(execFileName, machineID), "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, line := range splitLines(string(raw)) {
		n, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("machines: unreadable pid %q in a cgroup: %w", line, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		if i > start {
			out = append(out, s[start:i])
		}
		start = i + 1
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
