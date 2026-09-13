package machines

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/cron"
)

// Snapshots a volume takes on its own.
//
// # Why scheduled snapshots and not just manual ones
//
// Nobody takes a snapshot before the mistake. A manual snapshot protects
// against the failures you anticipated; a scheduled one protects against the
// ones you did not, which are the ones that happen. This is the difference
// between "you can roll back" and "you can roll back to a moment you happened
// to think of".
//
// # Why the owner host fires it
//
// A snapshot is a clone inside the volume's own filesystem, and a volume is
// mounted by exactly one host. So the host holding the mount is the only one
// that can take the snapshot, which makes the schedule a per-host loop over
// the volumes it holds rather than anything coordinated. Two hosts cannot both
// fire one volume's schedule, because only one of them has it.
//
// # Why retention is two numbers
//
// Keeping everything costs storage that grows without bound; keeping the last
// N loses the ability to go back further than N days. Keeping the newest N
// DAILIES plus the newest of each of the last M ISO WEEKS answers both
// questions -- recent resolution and historical reach -- in space that is
// bounded by N+M.

// snapshotFired remembers which minute each volume's schedule last fired on,
// so a loop that ticks several times inside one minute fires once.
type snapshotFired struct {
	mu  sync.Mutex
	at  map[string]time.Time
	now func() time.Time
}

func newSnapshotFired() *snapshotFired {
	return &snapshotFired{at: map[string]time.Time{}, now: time.Now}
}

// claim reports whether this volume's schedule should fire for this minute,
// and records that it did.
func (f *snapshotFired) claim(volumeID string, minute time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.at[volumeID]; ok && last.Equal(minute) {
		return false
	}
	f.at[volumeID] = minute
	return true
}

// snapshotDueVolumes takes the snapshots this minute calls for, and prunes.
//
// Called from the idle loop rather than given a ticker of its own: that loop
// already runs every few seconds over this host's machines, and a second timer
// would be a second thing to keep alive and a second thing to notice when it
// stops.
func (m *Manager) snapshotDueVolumes(ctx context.Context) {
	if m.opts.Volumes == nil {
		return
	}
	policies, err := m.opts.Store.ListVolumePolicies(ctx)
	if err != nil {
		slog.Warn("could not read volume snapshot schedules", "err", err)
		return
	}
	minute := time.Now().UTC().Truncate(time.Minute)

	for _, policy := range policies {
		if policy.Cron == "" {
			continue
		}
		v, err := m.opts.Store.GetVolume(ctx, policy.VolumeID)
		if err != nil || v.HostID != m.opts.HostID {
			// Another host's volume, or one that has gone. Its own host fires
			// its schedule; there is nothing to coordinate.
			continue
		}
		spec, err := cron.Parse(policy.Cron)
		if err != nil {
			slog.Warn("a volume's snapshot schedule cannot be read; it will never fire",
				"volume", policy.VolumeID, "cron", policy.Cron, "err", err)
			continue
		}
		if !spec.Matches(minute) || !m.snapshotFired.claim(policy.VolumeID, minute) {
			continue
		}

		if _, err := m.SnapshotVolume(ctx, policy.VolumeID); err != nil {
			slog.Error("a scheduled volume snapshot failed",
				"volume", policy.VolumeID, "err", err)
			continue
		}
		slog.Info("took a scheduled volume snapshot",
			"volume", policy.VolumeID, "cron", policy.Cron)

		// Pruned straight after, so retention is enforced at the moment the
		// set grows rather than by a second loop that could fall behind it.
		m.pruneSnapshots(ctx, policy.VolumeID, policy.KeepDaily, policy.KeepWeekly)
	}
}

// pruneSnapshots deletes what the retention policy does not keep.
func (m *Manager) pruneSnapshots(ctx context.Context, volumeID string, keepDaily, keepWeekly int) {
	have, err := m.opts.Volumes.ListSnapshots(volumeID)
	if err != nil {
		slog.Warn("could not list snapshots to prune", "volume", volumeID, "err", err)
		return
	}
	for _, stamp := range expiredSnapshots(have, keepDaily, keepWeekly) {
		if err := m.opts.Volumes.DeleteSnapshot(ctx, volumeID, stamp); err != nil {
			slog.Warn("could not delete an expired snapshot",
				"volume", volumeID, "snapshot", stamp, "err", err)
		}
	}
}

// expiredSnapshots is which of these to delete, given a retention policy.
//
// A pure function, so what a policy KEEPS can be tested exhaustively without a
// filesystem. That matters more here than in most places: the cost of a wrong
// answer is somebody's data, and it is discovered at the worst possible moment.
//
// Keeps the newest keepDaily snapshots outright, plus the newest of each of
// the keepWeekly most recent ISO weeks. Everything else goes. A policy with
// both numbers zero keeps EVERYTHING rather than deleting everything: an
// unset policy must never be read as "delete it all".
func expiredSnapshots(stamps []string, keepDaily, keepWeekly int) []string {
	if keepDaily <= 0 && keepWeekly <= 0 {
		return nil
	}
	// Newest first, which the stamps already sort as.
	sorted := append([]string(nil), stamps...)
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))

	keep := map[string]bool{}
	for i, stamp := range sorted {
		if i < keepDaily {
			keep[stamp] = true
		}
	}
	if keepWeekly > 0 {
		weeks := map[string]bool{}
		for _, stamp := range sorted {
			at, err := time.Parse("20060102T150405Z", stamp)
			if err != nil {
				// A name this code did not write. Kept rather than deleted:
				// deleting something unrecognised is how a retention policy
				// turns into data loss.
				keep[stamp] = true
				continue
			}
			year, week := at.ISOWeek()
			key := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") +
				"-" + itoa(week)
			if weeks[key] || len(weeks) >= keepWeekly {
				continue
			}
			weeks[key] = true
			keep[stamp] = true
		}
	}

	var out []string
	for _, stamp := range sorted {
		if !keep[stamp] {
			out = append(out, stamp)
		}
	}
	return out
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
