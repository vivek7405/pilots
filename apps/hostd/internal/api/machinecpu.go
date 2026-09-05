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
func (d Deps) startOf(ctx context.Context, id string) state.MachineCPU {
	view := d.machineCPU()
	if view == nil {
		return state.MachineCPU{}
	}
	row, _ := view.MachineCPU(ctx, id)
	return row
}
