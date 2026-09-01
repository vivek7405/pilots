package main

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// tenantInterval is how often the root-namespace filter is recomputed.
//
// Short, because it is what stands between one tenant and another and the
// window it is stale in is a window a new machine is unreachable in. Cheap,
// because the rules are only applied when they actually differ -- the tick
// itself is a map walk over local state.
const tenantInterval = 2 * time.Second

// fleetView is the cluster state the tenant filter and the DNS responder read.
//
// Deliberately the subscription cache in a fleet: both of these run on the
// data path, and a query to the corrosion agent per tick or per DNS request
// would put the control plane back in it -- the same defect the router's hot
// path had.
type fleetView interface {
	Machines() []state.Machine
	Hosts() []state.Host
}

// storeView is the single-box fleetView: a local SQLite read, with no agent
// and no subscription to read instead.
//
// It exists so that a lone host behaves like a fleet of one rather than like a
// different product. Without it, .internal and guest-to-guest traffic would
// work in a cluster and silently not on a single box, which is the worst place
// for that difference to hide -- it is where every developer meets the system
// first.
type storeView struct{ store state.Store }

func (v storeView) Machines() []state.Machine {
	rows, err := v.store.ListMachines(context.Background())
	if err != nil {
		slog.Error("could not read machines for the tenant filter", "err", err)
		return nil
	}
	return rows
}

func (v storeView) Hosts() []state.Host {
	rows, err := v.store.ListHosts(context.Background())
	if err != nil {
		return nil
	}
	return rows
}

// runTenantFilter keeps the root namespace's rules matching the fleet.
func runTenantFilter(ctx context.Context, hostID string, view fleetView, loc *mesh.Locator) {
	tick := time.NewTicker(tenantInterval)
	defer tick.Stop()

	var applied string
	for {
		rules := tenantRules(hostID, view, loc)
		if fingerprint := rules.Fingerprint(); fingerprint != applied {
			if err := netns.ApplyTenantFilter(rules); err != nil {
				// Left at the previous fingerprint deliberately, so the next
				// tick retries. A host that silently stopped reconciling this
				// keeps the rules it had, which get more wrong with every
				// machine that moves.
				slog.Error("could not apply the tenant filter; guest-to-guest "+
					"traffic is running on the previous rules", "err", err)
			} else {
				applied = fingerprint
				slog.Info("tenant filter reconciled",
					"local_machines", len(rules.Local), "apps", len(rules.Apps))
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// tenantRules turns the fleet's rows into the filter's desired state.
func tenantRules(hostID string, view fleetView, loc *mesh.Locator) netns.TenantRules {
	rules := netns.TenantRules{Apps: map[string][]netip.Addr{}}

	for _, m := range view.Machines() {
		addr, ok := loc.MachineAddress(m)
		if !ok {
			// Suspended, or owned by a host whose row has not arrived. Either
			// way it is not reachable and must not be in anyone's set.
			continue
		}
		if m.App != "" {
			rules.Apps[m.App] = append(rules.Apps[m.App], addr)
		}
		if m.HostID == hostID {
			rules.Local = append(rules.Local, netns.TenantMachine{
				SlotIdx: m.Slot, Addr: addr, App: m.App,
			})
		}
	}
	return rules
}
