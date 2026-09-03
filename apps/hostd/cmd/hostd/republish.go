package main

import (
	"context"
	"fmt"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// ownRowStore is the slice of state.Store republishing needs.
//
// Narrowed on purpose: the whole point of this function is that it writes
// ONLY rows this host owns, and a parameter that cannot reach a delete or a
// claim is a cheaper guarantee than a comment saying it does not.
type ownRowStore interface {
	ListMachines(ctx context.Context) ([]state.Machine, error)
	PutMachine(ctx context.Context, m *state.Machine, opts ...state.WriteOption) error
	ListVolumes(ctx context.Context) ([]state.Volume, error)
	PutVolume(ctx context.Context, v *state.Volume, opts ...state.WriteOption) error
}

// republishOwnRows re-writes every machine and volume row this host owns, so a
// change that never left the host is gossiped again.
//
// Why this exists. A write goes to the local replica and gossips from there. A
// host that was partitioned, wedged, or killed between the local write and the
// broadcast leaves peers holding a stale row, and nothing else in the system
// ever corrects it: anti-entropy reconciles what corrosion knows was written,
// not what a crashed process meant to write. Re-publishing on start is the
// cheapest repair, and it is the one fly reached for as well.
//
// Read from the REPLICA, never from local disk. The distinction is
// load-bearing. A row this host owns but the replica no longer shows means a
// rescuer has claimed the machine and now owns it; writing it back from a
// local state file would re-assert the old owner column, and last-write-wins
// would hand the machine back to a host that is not running it. Sourcing from
// the replica makes that impossible to express. The store's own guard is the
// second layer: a full-row write carries `WHERE machines.host_id = ?`, so a
// write aimed at another host's machine changes nothing.
//
// On start ONLY, not on a timer. cr-sqlite emits one change per column
// written, so a machines row is roughly two dozen changes; a per-minute timer
// across a fleet is continuous gossip for no benefit. The case being defended
// against is a write that never left this host, and a restart is precisely
// when that case exists and is re-sent.
//
// Tombstones are rows too: a delete that never gossiped is re-published by the
// same loop, which is what makes a machine destroyed just before a crash stay
// destroyed.
//
// The `tenancy` table belongs here too and is not here yet: it lands with #30,
// along with the Put that writes it. Adding it is one more filtered loop with
// the same shape, and ownRowStore is where its two methods go.
func republishOwnRows(ctx context.Context, hostID string, store ownRowStore) (int, error) {
	machines, err := store.ListMachines(ctx)
	if err != nil {
		return 0, fmt.Errorf("republish: list machines: %w", err)
	}
	n := 0
	for i := range machines {
		if machines[i].HostID != hostID {
			continue
		}
		// No write options. The owner guard is the safety here, and passing
		// an option to bypass it is exactly the mistake this loop must not
		// make: it would turn a repair into a fleet-wide overwrite.
		if err := store.PutMachine(ctx, &machines[i]); err != nil {
			return n, fmt.Errorf("republish machine %s: %w", machines[i].ID, err)
		}
		n++
	}

	volumes, err := store.ListVolumes(ctx)
	if err != nil {
		return n, fmt.Errorf("republish: list volumes: %w", err)
	}
	for i := range volumes {
		if volumes[i].HostID != hostID {
			continue
		}
		if err := store.PutVolume(ctx, &volumes[i]); err != nil {
			return n, fmt.Errorf("republish volume %s: %w", volumes[i].ID, err)
		}
		n++
	}
	return n, nil
}
