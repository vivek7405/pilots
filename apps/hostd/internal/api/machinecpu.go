package api

import (
	"context"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// MachineCPUView answers how a machine last came up: a restore of its memory
// image, the boot a create with an image or a volume pays once, or a cold boot
// -- a restore downgraded because no host of the image's CPU vendor was alive.
//
// An interface for the same reason TenancyView is one: in a fleet the answer
// is a map read off the subscription cache, on a single box it is a query, and
// the handlers must not be able to tell. Both read LOCAL state only.
type MachineCPUView interface {
	MachineCPU(ctx context.Context, id string) (state.MachineCPU, bool)
}

// StoreMachineCPU answers from the state store. Used on a single box and by
// the tests, where there is no subscription cache to read.
func StoreMachineCPU(st state.Store) MachineCPUView { return storeMachineCPU{st} }

type storeMachineCPU struct{ st state.Store }

func (v storeMachineCPU) MachineCPU(ctx context.Context, id string) (state.MachineCPU, bool) {
	row, err := v.st.GetMachineCPU(ctx, id)
	if err != nil {
		return state.MachineCPU{}, false
	}
	return *row, true
}

// machineCPU returns the configured view, or one over the store.
//
// The fallback keeps a Deps built without one correct rather than panicking,
// exactly as tenancy() does: a machine with no recorded start reports none,
// which is what every machine created before this table looks like.
func (d Deps) machineCPU() MachineCPUView {
	if d.MachineCPU != nil {
		return d.MachineCPU
	}
	if d.Store == nil {
		return nil
	}
	return StoreMachineCPU(d.Store)
}

// startOf reads a machine's last start, or the zero row when nothing recorded
// one. Absent is normal, not an error: it is what a machine that predates this
// table reads as, and the API omits both fields.
//
// # Why both are consulted, and the newer wins
//
// On a fleet the view is the subscription cache, which lags its own host's
// writes by however long the subscription takes to deliver them. A CREATE
// writes this row and then builds its response, so it was reading a cache that
// had not seen the write yet and answering with no last_start at all -- for the
// one machine whose start it had just recorded. A second later a GET showed it.
//
// Falling back to the store only when the cache MISSED fixed the create and
// left a worse bug behind it. Every start after the first finds a non-empty
// LastStart already in the cache, so the fallback never ran and the API served
// the PREVIOUS start: a wake answered with the start before it, indefinitely
// one behind. That is invisible while a machine keeps restoring, and wrong the
// moment the kind changes -- a machine that had just cold-booted reported
// "restore", and its next ordinary restore reported "cold_boot", which reads
// exactly like a machine that reboots forever.
//
// So both are consulted and the newer row wins. The store is local,
// authoritative, already open, and has Corrosion's applied rows from every
// peer too, so it is never worse than the cache -- it is simply the only one
// that has seen THIS host's write before the subscription delivers it back.
// The cache still answers for machines this host does not own, which is what
// it is for.
//
// Compared on LastStartAt, NOT UpdatedAt: the subscription selects only id,
// kind, vendor, last_start and last_start_at, so a cached row's UpdatedAt is
// always zero and comparing on it would silently always prefer the store.
func (d Deps) startOf(ctx context.Context, id string) state.MachineCPU {
	var best state.MachineCPU
	if view := d.machineCPU(); view != nil {
		if row, ok := view.MachineCPU(ctx, id); ok {
			best = row
		}
	}
	if d.Store != nil {
		if row, err := d.Store.GetMachineCPU(ctx, id); err == nil && row != nil &&
			row.LastStartAt >= best.LastStartAt {
			best = *row
		}
	}
	return best
}
