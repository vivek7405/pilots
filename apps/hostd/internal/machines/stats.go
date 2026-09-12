package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// What one machine is actually using.
//
// # Why this is read on the owner and not scraped fleet-wide
//
// The numbers live in a cgroup on one host: the host running the machine. A
// fleet-wide table of them would be one CRDT row per running machine per tick,
// gossiped to every host, to serve a number nobody reads until somebody opens a
// page. That is a write load priced in the prior art rather than guessed at,
// and it buys nothing a pull cannot.
//
// So the owner reads its own, and a host asked about somebody else's forwards.
//
// # Why the CPU counter is persisted
//
// A counter that goes DOWN is worse than no counter: every rate over it goes
// negative or enormous, and every alert built on it fires on a suspend. A
// machine's cgroup is destroyed when it suspends and recreated on wake, so the
// kernel's own total restarts at zero. The last total is written beside the
// machine's state and added back afterwards, which is the same trick the uffd
// metrics already use for a handler that is replaced.

// statsFile is where a machine's CPU total survives its cgroup.
const statsFile = "stats.json"

type persistedStats struct {
	// CPUUsec is the total at the moment the cgroup was last readable.
	CPUUsec int64 `json:"cpu_usec"`
}

// Stats samples one machine this host owns.
//
// A machine with no cgroup -- suspended, or on a host with no cgroup v2 -- is
// not an error: it reports its persisted CPU total and no memory, which is
// exactly what a suspended machine is using. Returning an error there would
// make an ordinary state look like a broken one on every page that asks.
func (m *Manager) Stats(ctx context.Context, id string) (*api.Stats, error) {
	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.HostID != "" && row.HostID != m.opts.HostID {
		return nil, fmt.Errorf("machines: %s runs on %s, not here: %w", id, row.HostID, state.ErrNotOwner)
	}

	out := &api.Stats{
		SampledAt:        time.Now(),
		MemoryLimitBytes: int64(row.MemMiB) << 20,
	}
	carried := m.carriedCPU(id)

	dir := m.cgroupOf(id)
	usec, err := readCPUUsec(dir)
	if err != nil {
		// No cgroup to read. The machine is suspended, stopped, or this host
		// does not account that way; either way the persisted total is the
		// honest answer and memory is zero.
		out.CPUSeconds = float64(carried) / 1e6
		return out, nil
	}
	out.CPUSeconds = float64(carried+usec) / 1e6
	out.MemoryBytes = readInt(filepath.Join(dir, "memory.current"))
	if limit := readInt(filepath.Join(dir, "memory.max")); limit > 0 {
		// The cgroup's own ceiling wins where there is one: it is what the
		// kernel will actually enforce, and a row that disagrees with the
		// kernel is the row that is wrong.
		out.MemoryLimitBytes = limit
	}
	return out, nil
}

// cgroupOf is the machine's slice, built the same way every other caller builds
// it so the two cannot drift.
func (m *Manager) cgroupOf(id string) string {
	return machineCgroup(filepath.Base(m.opts.FCConfig.FirecrackerBin), id)
}

// carriedCPU is the total this machine accumulated before its current cgroup.
func (m *Manager) carriedCPU(id string) int64 {
	raw, err := os.ReadFile(filepath.Join(m.stateDir(id), statsFile))
	if err != nil {
		return 0
	}
	var saved persistedStats
	if err := json.Unmarshal(raw, &saved); err != nil {
		return 0
	}
	return saved.CPUUsec
}

// PersistCPU records the machine's CPU total before its cgroup goes away.
//
// Called on the paths that end a cgroup's life -- suspend, stop, checkpoint --
// because after them the kernel's counter restarts at zero and a counter that
// restarts is a counter nothing can rate.
//
// Best effort, and deliberately so: refusing to suspend a machine because a
// bookkeeping file could not be written would trade a real operation for a
// number.
func (m *Manager) PersistCPU(id string) {
	usec, err := readCPUUsec(m.cgroupOf(id))
	if err != nil {
		return
	}
	total := m.carriedCPU(id) + usec
	raw, err := json.Marshal(persistedStats{CPUUsec: total})
	if err != nil {
		return
	}
	dir := m.stateDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, statsFile), raw, 0o644)
}

// readCPUUsec reads the cgroup's total CPU time in microseconds.
//
// cpu.stat rather than cpuacct: this is cgroup v2, where the first line is
// `usage_usec <n>` and covers user and system together, which is the number a
// person means by "how much CPU has this used".
func readCPUUsec(dir string) (int64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || key != "usage_usec" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, errors.New("machines: cpu.stat carries no usage_usec")
}

// readInt reads a cgroup file holding one number.
//
// "max" is the kernel's word for no limit, and it becomes 0 here rather than an
// error: a machine with no memory ceiling has no ceiling to report, which is
// different from a read that failed.
func readInt(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(string(raw))
	if text == "max" {
		return 0
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
