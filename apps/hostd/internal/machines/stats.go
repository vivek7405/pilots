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
	"syscall"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/api"
	"github.com/pilotsrun/pilots/hostd/internal/state"
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
// machine's cgroup does not survive a destroy, a reap, or a host restart, and
// the kernel's total starts at zero in whatever slice comes next. The last
// total is written beside the machine's state and added back afterwards, which
// is the same trick the uffd metrics already use for a handler that is
// replaced.
//
// What the cgroup DOES survive is a suspend. Nothing on that path removes it,
// which is worth knowing because it is the opposite of what this comment used
// to claim, and the wrong version is what led Stats to treat a present cgroup
// as a running machine and report a suspended one's residual charge as live
// memory.

// statsFile is where a machine's CPU total survives its cgroup.
const statsFile = "stats.json"

type persistedStats struct {
	// CPUUsec is the total at the moment the cgroup was last readable.
	CPUUsec int64 `json:"cpu_usec"`
	// FoldedUsec is how much of CPUUsec came from the cgroup named by
	// FoldedIno, and FoldedIno is that cgroup's inode number.
	//
	// Together they make PersistCPU idempotent. The counter in a cgroup is
	// CUMULATIVE and the cgroup OUTLIVES the machine's processes -- suspend
	// kills the VMM and leaves the slice in place, only Destroy and the reaper
	// remove it -- so a second PersistCPU before the machine next runs read the
	// same usec out of the same file and added it to a total that already
	// contained it. Suspend then stop, suspend then checkpoint, or a suspend
	// retried after a transient failure each charged the machine twice for its
	// last waking period. Measured: one period of 100 s read back as 200 s
	// after the second call and 300 s after the third, because every repeat
	// adds that whole period again.
	//
	// The inode is the discriminator, because the path is not. A cgroup
	// directory removed and recreated at the same path gets a fresh inode from
	// kernfs, which is exactly the event that resets the counter to zero. Same
	// inode means the same waking period, so the previous fold is replaced
	// rather than added to; a different inode means the counter restarted, so
	// the running total stands and the new reading is added to it.
	//
	// Absent in a file written before this field existed, which reads as
	// FoldedIno 0 and never matches a real inode: an old file therefore keeps
	// the old behaviour of adding, which is the direction that cannot lose
	// somebody's accumulated total.
	FoldedUsec int64  `json:"folded_usec,omitempty"`
	FoldedIno  uint64 `json:"folded_ino,omitempty"`
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

	// The cgroup OUTLIVES the machine's processes. Suspend kills the VMM and
	// leaves the slice in place -- only Destroy and the reaper remove it --
	// so "the directory is there" does not mean "the machine is running", and
	// reading it as if it did was two wrong numbers rather than one.
	//
	// An empty cgroup.procs is the physical fact, and it is what is checked
	// here rather than the row's state: a row says what the fleet last agreed,
	// and a VMM that died without hostd writing one still reads as running.
	// This says what is true on this host at this instant.
	dir := m.cgroupOf(id)
	usec, err := readCPUUsec(dir)
	if err != nil || !cgroupHasProcs(dir) {
		// Nothing is running here. The persisted total is the whole answer:
		//
		// CPU is the carried total ALONE, not carried+usec. Suspend calls
		// PersistCPU, which writes carried+usec into the machine's state dir,
		// and the cgroup it read that usec from is still sitting there with
		// the same number in it. Adding it again charges the machine twice for
		// its last waking period.
		//
		// Memory is left at zero. With no process there is nothing resident;
		// memory.current still reports the page cache and slab the kernel has
		// not reclaimed, which on the rig was 4.6 MiB and read exactly like a
		// small machine running.
		//
		// The LIMIT stays the row's rather than blinking to zero: it is what
		// the machine will be held to when it wakes, which is still a true
		// thing to say about a machine that is asleep.
		out.CPUSeconds = float64(m.carriedCPU(id)) / 1e6
		return out, nil
	}
	// carriedBefore, not carriedCPU: a PersistCPU that ran while this very
	// cgroup was alive (checkpoint takes one without ending the machine) has
	// already folded part of this usec into the persisted total, and adding the
	// whole reading to it reports the same time twice.
	out.CPUSeconds = float64(m.carriedBefore(id, cgroupIno(dir), usec)+usec) / 1e6
	out.MemoryBytes = readInt(filepath.Join(dir, "memory.current"))

	// The LIMIT stays the row's, and memory.max is deliberately not read.
	//
	// memory.max is the guest's RAM plus 128 MiB of headroom for Firecracker's
	// own allocations (see vmmOverheadMiB), so reporting it told the owner of a
	// 512 MiB machine that their limit was 640 MiB. That overhead is the
	// hypervisor's slice and not memory a guest can ever allocate: a guest
	// reaching 640 was OOM-killed at 512.
	//
	// It was wrong downstream too. The CLI warns at 90% of the limit, which
	// against a 640 MiB denominator is 576 MiB -- unreachable, so the warning
	// never fired. The dashboard draws its fill bar the same way, so a full
	// machine read 80%.
	//
	// And memory.max is written once by the jailer at boot, so after a vertical
	// resize it describes the size the machine used to be.
	return out, nil
}

// cgroupOf is the machine's slice, built the same way every other caller builds
// it so the two cannot drift.
func (m *Manager) cgroupOf(id string) string {
	return machineCgroup(filepath.Base(m.opts.FCConfig.FirecrackerBin), id)
}

// carriedCPU is the machine's persisted total: everything it has accumulated,
// including whatever the last PersistCPU folded in.
//
// The right number for a machine that is NOT running, where nothing is left to
// add to it.
func (m *Manager) carriedCPU(id string) int64 {
	saved, ok := m.savedStats(id)
	if !ok {
		return 0
	}
	return saved.CPUUsec
}

// carriedBefore is the persisted total MINUS anything already folded in from
// the cgroup this reading of usec came from.
//
// The right number to add a live reading to. Reading the whole persisted total
// instead double-counted every microsecond a PersistCPU had already recorded
// from the cgroup still sitting there, which is what made a machine's reported
// CPU double on a second suspend.
//
// TWO conditions, because either one alone has a hole:
//
//   - The inode must match. A cgroup removed and recreated at the same path is
//     a new waking period whose counter restarted at zero, and its time is
//     owed on TOP of the total rather than in place of part of it.
//   - The counter must not have gone BACKWARDS. cpu.stat is monotonic within
//     one cgroup, so a reading below what was folded means the counter
//     restarted -- and kernfs allocates inode numbers from an ida that can
//     hand a freed number back, so a recreated slice can land on the inode its
//     predecessor had. Without this the reused inode would be read as the same
//     period and a whole waking period discarded.
func (m *Manager) carriedBefore(id string, ino uint64, usec int64) int64 {
	saved, ok := m.savedStats(id)
	if !ok {
		return 0
	}
	if ino != 0 && saved.FoldedIno == ino && usec >= saved.FoldedUsec {
		return saved.CPUUsec - saved.FoldedUsec
	}
	return saved.CPUUsec
}

// savedStats reads the machine's persisted record. Absent or unreadable is not
// an error: a machine that has never been suspended has no file.
func (m *Manager) savedStats(id string) (persistedStats, bool) {
	raw, err := os.ReadFile(filepath.Join(m.stateDir(id), statsFile))
	if err != nil {
		return persistedStats{}, false
	}
	var saved persistedStats
	if err := json.Unmarshal(raw, &saved); err != nil {
		return persistedStats{}, false
	}
	return saved, true
}

// cgroupIno identifies one generation of a cgroup directory.
//
// A cgroup removed and recreated at the same path gets a fresh inode, and that
// is precisely the event that resets cpu.stat to zero -- so the inode answers
// "is this the same waking period?", which the path cannot. Zero when it
// cannot be read, which never matches a recorded inode.
func cgroupIno(dir string) uint64 {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Ino)
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
	dir := m.cgroupOf(id)
	usec, err := readCPUUsec(dir)
	if err != nil {
		return
	}
	// IDEMPOTENT. Called twice against one cgroup -- suspend then stop, or a
	// suspend retried -- the second call REPLACES the first's contribution
	// rather than adding the same microseconds again.
	ino := cgroupIno(dir)
	total := m.carriedBefore(id, ino, usec) + usec
	raw, err := json.Marshal(persistedStats{
		CPUUsec: total, FoldedUsec: usec, FoldedIno: ino,
	})
	if err != nil {
		return
	}
	stateDir := m.stateDir(id)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(stateDir, statsFile), raw, 0o644)
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
