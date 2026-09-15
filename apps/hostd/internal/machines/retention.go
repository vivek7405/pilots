package machines

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Which checkpoints are worth keeping.
//
// # The problem
//
// A sandbox an agent checkpoints after every message accumulates checkpoints
// forever. Each one is bytes in object storage, metered now (see
// snapshotmeter.go) and paid for by somebody, and nothing ever removed one.
// The machine is destroyed eventually and takes them with it, but a long-lived
// sandbox is exactly the case where that never happens.
//
// # The rule
//
// Tiered thinning, the shape every backup tool converged on because it matches
// what people actually want from history: everything recent, then progressively
// less of it, forever. Concretely, keeping
//
//   - every checkpoint younger than All (an hour): the undo window,
//   - the newest per hour under Hourly (a day): today, at hourly resolution,
//   - the newest per day under Daily (a week): this week, at daily resolution,
//   - and the newest checkpoint always, whatever its age.
//
// A checkpoint somebody NAMED is never removed. A comment is a person saying
// "this one matters", and a retention policy that discarded it would be the
// policy deciding it knew better. Release snapshots are likewise kept: they are
// what a rollback restores, and thinning them would quietly remove the ability
// to roll back.
//
// # Why thinning cannot break a survivor
//
// A checkpoint's parent is the TEMPLATE it diffs against, not the previous
// checkpoint (see the chunkify call in Manager.Checkpoint). So the chain is
// flat: removing any checkpoint leaves every other one restorable. If the chain
// were sequential this would need a keep-the-ancestors pass, and getting that
// wrong would be silent data loss rather than a failed restore.

// Retention is the tiered policy. Zero values mean the defaults below.
type Retention struct {
	// All keeps every checkpoint younger than this.
	All time.Duration
	// Hourly keeps the newest checkpoint per hour, up to this age.
	Hourly time.Duration
	// Daily keeps the newest per day, up to this age. Past it, only the newest
	// checkpoint overall and anything named survives.
	Daily time.Duration
}

// DefaultRetention is an hour of everything, a day of hourlies, a week of
// dailies. Generous enough that nobody notices it until it has saved them
// money, which is the right setting for a default nobody will tune.
var DefaultRetention = Retention{
	All:    time.Hour,
	Hourly: 24 * time.Hour,
	Daily:  7 * 24 * time.Hour,
}

func (r Retention) orDefaults() Retention {
	if r.All == 0 {
		r.All = DefaultRetention.All
	}
	if r.Hourly == 0 {
		r.Hourly = DefaultRetention.Hourly
	}
	if r.Daily == 0 {
		r.Daily = DefaultRetention.Daily
	}
	return r
}

// keepSet names every checkpoint that survives the policy.
//
// A pure function over the rows, so the policy is testable without a store, a
// bucket or a machine. That matters more here than usual: every bug in it
// deletes a customer's data, and the only way to be sure about a tiered rule
// is to run it over a constructed history and read the answer.
func keepSet(now time.Time, cks []state.Checkpoint, r Retention) map[string]bool {
	r = r.orDefaults()
	keep := map[string]bool{}
	if len(cks) == 0 {
		return keep
	}

	// Newest first, so "the newest in this bucket" is the first one seen.
	sorted := append([]state.Checkpoint(nil), cks...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt != sorted[j].CreatedAt {
			return sorted[i].CreatedAt > sorted[j].CreatedAt
		}
		// A stable tie-break, so two hosts reading the same rows thin them the
		// same way. Seq descends with age, so the higher one is newer.
		return sorted[i].Seq > sorted[j].Seq
	})

	// Always the newest, whatever the policy says. A machine whose every
	// checkpoint aged out would have nothing to roll back to, which is a worse
	// outcome than the bytes.
	keep[sorted[0].ID] = true

	seenHour := map[int64]bool{}
	seenDay := map[int64]bool{}
	for _, ck := range sorted {
		// Somebody named it. A comment is a person saying this one matters.
		if ck.Comment != "" {
			keep[ck.ID] = true
			continue
		}
		age := now.Sub(time.Unix(ck.CreatedAt, 0))
		switch {
		case age < 0:
			// Created in the future, which means a clock that disagrees. Keep
			// it: deleting data because of a clock skew is not a trade worth
			// making.
			keep[ck.ID] = true
		case age <= r.All:
			keep[ck.ID] = true
		case age <= r.Hourly:
			bucket := ck.CreatedAt / 3600
			if !seenHour[bucket] {
				seenHour[bucket] = true
				keep[ck.ID] = true
			}
		case age <= r.Daily:
			bucket := ck.CreatedAt / 86400
			if !seenDay[bucket] {
				seenDay[bucket] = true
				keep[ck.ID] = true
			}
		}
	}
	return keep
}

// ExpireCheckpoints thins the checkpoints of every machine this host owns.
//
// Driven by the reaper's tick, which is the loop that already exists for
// "clean up what nothing is using". Under the machine lock, so it cannot race
// a checkpoint being taken or a restore reading one.
//
// Deletes exactly what destroy deletes for ONE checkpoint, through the same
// calls, so there is one description of what a checkpoint's remains are. A
// second one here would be a second thing to keep correct, and the failure
// mode of getting it wrong is an object nobody ever deletes.
func (m *Manager) ExpireCheckpoints(ctx context.Context) int {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return 0
	}
	removed := 0
	for _, row := range rows {
		if row.HostID != m.opts.HostID || row.State == state.StateDestroyed {
			continue
		}
		removed += m.expireOne(ctx, row.ID)
	}
	return removed
}

// expireOne thins one machine's checkpoints and re-meters what is left.
func (m *Manager) expireOne(ctx context.Context, machineID string) int {
	lock := m.lockFor(machineID)
	lock.Lock()
	defer lock.Unlock()

	cks, err := m.opts.Store.ListCheckpoints(ctx, machineID)
	if err != nil || len(cks) == 0 {
		return 0
	}
	gone := expired(time.Now(), cks, m.opts.Retention)
	if len(gone) == 0 {
		return 0
	}

	removed := 0
	for _, ck := range gone {
		if err := m.deleteCheckpoint(ctx, machineID, ck); err != nil {
			slog.Warn("could not expire a checkpoint; it will be retried on the "+
				"next pass", "checkpoint", ck.ID, "machine", machineID, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("expired checkpoints past the retention policy",
			"machine", machineID, "removed", removed, "kept", len(cks)-removed)
		// The bytes are gone, so the meter has to stop charging for them.
		m.remeterSnapshots(ctx, machineID)
	}
	return removed
}

// deleteCheckpoint removes one checkpoint's objects, builds, rows and local
// staging directory.
func (m *Manager) deleteCheckpoint(ctx context.Context, machineID string, ck state.Checkpoint) error {
	var errs []error
	if deleter, ok := m.opts.Uploader.(interface {
		Delete(ctx context.Context, key string) error
	}); ok {
		if err := deleter.Delete(ctx, checkpointSnapKey(machineID, ck.ID)); err != nil {
			errs = append(errs, err)
		}
	}
	// The template's builds are shared and are never named by a checkpoint
	// (see deleteRemoteState), so this cannot take one.
	m.discardBuilds(ctx, ck.MemBuildID, ck.RootfsBuildID)

	// The cpu row FIRST, for the reason Checkpoint writes it last: a
	// replicated store finds this row's writer through checkpoints.machine_id,
	// so a row deleted after its checkpoint has nothing left to prove
	// ownership with and stays forever.
	if err := m.opts.Store.DeleteMachineCPU(ctx, ck.ID); err != nil {
		errs = append(errs, err)
	}
	if err := m.opts.Store.DeleteCheckpoint(ctx, ck.ID); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(m.checkpointDir(machineID, ck.ID)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// expired is the complement of keepSet: what may be deleted, oldest first so a
// run that is interrupted has made the most progress.
func expired(now time.Time, cks []state.Checkpoint, r Retention) []state.Checkpoint {
	keep := keepSet(now, cks, r)
	var out []state.Checkpoint
	for _, ck := range cks {
		if !keep[ck.ID] {
			out = append(out, ck)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}
