package dns

import (
	"math/rand/v2"
	"net/netip"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// FleetView is the cluster state a name is resolved against.
//
// The local subscription cache, always. A query to the corrosion agent per DNS
// request would put the control plane on the data path -- a guest could not
// find its database while the local agent was restarting, which is precisely
// the moment nothing else has gone wrong yet.
type FleetView interface {
	Machines() []state.Machine
	// Services is the name half of service discovery. A replica's machine row
	// carries the service id but never the service NAME, so without this a
	// name like db.internal has nothing to match against.
	Services() []state.Service
}

// FleetResolver answers .internal from replicated rows.
type FleetResolver struct {
	view FleetView
	loc  *mesh.Locator
}

// NewFleetResolver returns a resolver over a fleet view.
func NewFleetResolver(view FleetView, loc *mesh.Locator) *FleetResolver {
	return &FleetResolver{view: view, loc: loc}
}

// Resolve returns the addresses a name points at, for the machine that asked.
//
// Scoped to the querying machine's app, and the scope comes from the ROW, not
// from the packet: the machine id is known from the socket the query arrived
// on, which is inside that machine's namespace and nowhere else. A guest
// cannot ask for another app's names by claiming to be in it.
//
// A machine with no app resolves nothing. That is the right default for a
// sandbox nobody grouped: it can be reached by name from nowhere, and it can
// reach nothing by name.
func (r *FleetResolver) Resolve(q Query) []netip.Addr {
	if q.Name == "" {
		return nil
	}

	rows := r.view.Machines()

	var asker state.Machine
	for _, m := range rows {
		if m.ID == q.MachineID {
			asker = m
			break
		}
	}
	if asker.App == "" {
		return nil
	}

	var (
		here  []netip.Addr
		there []netip.Addr
	)
	add := func(m state.Machine) {
		addr, ok := r.loc.MachineAddress(m)
		if !ok {
			return
		}
		if m.HostID == asker.HostID {
			here = append(here, addr)
		} else {
			there = append(there, addr)
		}
	}

	for _, m := range rows {
		if m.Name != q.Name || m.App != asker.App || !healthy(m) {
			continue
		}
		add(m)
	}

	// A SERVICE NAME, when no machine claimed one.
	//
	// Machine names win a collision. Not because a machine is the better
	// answer, but because it was the answer before services existed and a
	// name that quietly changes what it points at is worse than either
	// choice. Replica names are generated (amber-lagoon-x9f2), so the
	// collision needs a machine someone named by hand after a service.
	//
	// This is the layer a multi-service app actually addresses. A replica's
	// name is generated and is replaced on every rollout, so nothing an
	// application could write in a config file would survive a deploy --
	// which is why postgres://db.internal:5432 has to resolve here and not
	// through the machine path above.
	if len(here) == 0 && len(there) == 0 {
		for _, id := range r.serviceIDs(q.Name, asker.App) {
			for _, m := range rows {
				if m.ServiceID != id || !healthy(m) {
					continue
				}
				add(m)
			}
		}
	}

	// nearest. prefers a machine on the same host: a veth-to-veth hop instead
	// of a trip through WireGuard. It FALLS BACK rather than answering nothing,
	// because the alternative is a name that resolves until the local replica
	// is rescued away and then does not.
	if q.Mode == ModeNearest && len(here) > 0 {
		return shuffled(here)
	}
	return shuffled(append(here, there...))
}

// healthy is what "filtered to healthy" can mean today.
//
// Running, and not a tombstone. There is a services.health column in the
// schema and nothing writes it until the rollout work lands, so claiming to
// filter on a health check here would be claiming something untrue. A machine
// that is suspended has no slot and is filtered out a step later anyway; this
// catches the ones that are stopped or errored and still hold one.
func healthy(m state.Machine) bool {
	if m.State == "running" {
		return true
	}
	// A SUSPENDED SERVICE REPLICA still resolves.
	//
	// It kept its slot, so it kept its address, and traffic to that address is
	// counted in the root namespace and wakes it. Dropping it from DNS instead
	// is what made min_machines_running: 0 unusable for anything another
	// service depends on: an inbound HTTP request to a suspended machine is
	// held while the router wakes it, but a peer asking for the same machine
	// by name got NXDOMAIN and a connection error.
	//
	// Scoped to service REPLICAS on purpose -- a machine a rollout placed,
	// which is what ReleaseID records. A suspended sandbox has no slot
	// and nothing waiting to bring it back, so answering for it would point
	// traffic at an address with nothing behind it and no way to fix that.
	return m.State == "suspended" && m.ServiceID != "" && m.ReleaseID != "" && m.Slot > 0
}

// serviceIDs returns the ids of the services an app names q.
//
// Scoped to the asker's app for the same reason machine names are: the scope
// comes from the asker's ROW, so a guest cannot reach another app's database
// by naming it. Returns every match rather than the first -- corrosion cannot
// enforce uniqueness, so two hosts that disagreed during a membership change
// can each hold a service row with this name, and answering from only one of
// them would make the name resolve to half its replicas.
func (r *FleetResolver) serviceIDs(name, app string) []string {
	var ids []string
	for _, svc := range r.view.Services() {
		if svc.Name == name && svc.App == app {
			ids = append(ids, svc.ID)
		}
	}
	return ids
}

// shuffled spreads clients that take the first answer across the machines
// behind a name. Without it every caller in an app talks to whichever replica
// the map walk happened to yield first.
func shuffled(addrs []netip.Addr) []netip.Addr {
	if len(addrs) < 2 {
		return addrs
	}
	rand.Shuffle(len(addrs), func(i, j int) { addrs[i], addrs[j] = addrs[j], addrs[i] })
	return addrs
}
