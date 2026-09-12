package api

import (
	"context"

	"github.com/vivek7405/pilots/hostd/internal/state"
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
// # Why a miss falls back to the store
//
// On a fleet the view is the subscription cache, which lags its own host's
// writes by however long the subscription takes to deliver them. A CREATE
// writes this row and then builds its response, so it was reading a cache that
// had not seen the write yet and answering with no last_start at all -- for the
// one machine whose start it had just recorded. A second later a GET showed it.
//
// A client cannot tell "this machine has no recorded start" from "ask again in
// a moment", so it reads the create as a cold boot that never happened. The
// store is local, authoritative and already open; consulting it on a miss costs
// one query in the rare case and removes the race entirely.
//
// Cache FIRST, because on a fleet most reads are for machines this host does
// not own and the cache is the only place their row is.
func (d Deps) startOf(ctx context.Context, id string) state.MachineCPU {
	view := d.machineCPU()
	if view == nil {
		return state.MachineCPU{}
	}
	if row, ok := view.MachineCPU(ctx, id); ok && row.LastStart != "" {
		return row
	}
	if d.Store == nil {
		return state.MachineCPU{}
	}
	row, err := d.Store.GetMachineCPU(ctx, id)
	if err != nil || row == nil {
		return state.MachineCPU{}
	}
	return *row
}
