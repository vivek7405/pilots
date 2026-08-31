package machines

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Reaper timings.
//
// The age guard is the important one: a machine that is mid-create has a live
// Firecracker but has not yet written its row, and reaping it would destroy a
// machine the caller is still waiting on. A minute is far longer than a create
// takes and far shorter than anyone would tolerate an orphan lingering.
//
// The loop is slow because killing a Firecracker is destructive and a false
// positive is visible to a user; there is no hurry.
const (
	reaperMinAge   = 60 * time.Second
	reaperInterval = 5 * time.Minute
)

// RunReaper kills Firecracker processes this host has no record of, until ctx
// ends.
//
// Orphans happen: hostd can be SIGKILLed between spawning a machine and
// recording it, or a destroy can fail partway. Without a sweep they hold their
// memory, their cgroup and their network slot indefinitely, and the only
// remedy is a human with a terminal.
func (m *Manager) RunReaper(ctx context.Context) {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapOrphans(ctx)
		}
	}
}

func (m *Manager) reapOrphans(ctx context.Context) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		slog.Error("reaper could not list machines", "err", err)
		return
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[r.ID] = true
	}

	for _, p := range firecrackerProcesses() {
		if known[p.machineID] {
			continue
		}
		if p.age < reaperMinAge {
			// Probably a create still in flight.
			continue
		}
		slog.Warn("reaping an orphaned firecracker with no machine record",
			"pid", p.pid, "machine", p.machineID, "age", p.age)
		if err := unix.Kill(p.pid, unix.SIGKILL); err != nil {
			slog.Error("could not reap orphan", "pid", p.pid, "err", err)
		}
	}
}

type fcProcess struct {
	pid       int
	machineID string
	age       time.Duration
}

// firecrackerProcesses finds running Firecrackers and the machine each claims
// to be, read from the --id it was started with.
func firecrackerProcesses() []fcProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var out []fcProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "firecracker" {
			continue
		}

		id := machineIDFromCmdline(filepath.Join("/proc", e.Name(), "cmdline"))
		if id == "" {
			continue
		}

		age := time.Duration(0)
		if st, err := os.Stat(filepath.Join("/proc", e.Name())); err == nil {
			age = time.Since(st.ModTime())
		}
		out = append(out, fcProcess{pid: pid, machineID: id, age: age})
	}
	return out
}

// machineIDFromCmdline reads the --id Firecracker was started with. The
// jailer passes it through, so it is the machine's own identifier.
func machineIDFromCmdline(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	args := strings.Split(string(raw), "\x00")
	for i, a := range args {
		if a == "--id" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
