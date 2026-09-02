package dns

import (
	"net/netip"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeFleet is a fixed set of rows, standing in for the subscription cache.
type fakeFleet struct {
	machines []state.Machine
	hosts    []state.Host
	services []state.Service
}

func (f fakeFleet) Machines() []state.Machine { return f.machines }
func (f fakeFleet) Hosts() []state.Host       { return f.hosts }
func (f fakeFleet) Services() []state.Service { return f.services }

// keyFor makes a deterministic host key, so a test can assert the address a
// name resolves to rather than only that it resolved to something.
func keyFor(seed byte) wgtypes.Key {
	var k wgtypes.Key
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func testFleet(t *testing.T) (*FleetResolver, fakeFleet, wgtypes.Key, wgtypes.Key) {
	t.Helper()

	selfKey, peerKey := keyFor(1), keyFor(90)
	fleet := fakeFleet{
		hosts: []state.Host{
			{ID: "host-a", WGPubKey: selfKey.String()},
			{ID: "host-b", WGPubKey: peerKey.String()},
		},
		machines: []state.Machine{
			{ID: "m-asker", Name: "web", HostID: "host-a", State: "running", App: "shop", Slot: 1},
			{ID: "m-db-a", Name: "db", HostID: "host-a", State: "running", App: "shop", Slot: 2},
			{ID: "m-db-b", Name: "db", HostID: "host-b", State: "running", App: "shop", Slot: 5},
			// Same name, different app. This is the one that must never come
			// back: a name is only a name inside an app.
			{ID: "m-db-x", Name: "db", HostID: "host-b", State: "running", App: "other", Slot: 6},
			// Suspended: no slot, therefore no address, therefore no answer.
			{ID: "m-cache", Name: "cache", HostID: "host-a", State: "suspended", App: "shop", Slot: 0},
			// Ungrouped, and reachable from nowhere.
			{ID: "m-loose", Name: "loose", HostID: "host-a", State: "running", Slot: 9},
		},
	}
	return NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet)), fleet, selfKey, peerKey
}

// A machine in one app must not be able to find a machine in another, even
// when they share a name -- which they will, because "db" is what everyone
// calls their database.
func TestResolutionIsScopedToTheAskersApp(t *testing.T) {
	r, _, selfKey, peerKey := testFleet(t)

	got := r.Resolve(Query{MachineID: "m-asker", Name: "db"})
	if len(got) != 2 {
		t.Fatalf("resolved %d addresses, want the two in this app: %v", len(got), got)
	}

	want := map[netip.Addr]bool{}
	for _, a := range []struct {
		key  wgtypes.Key
		slot int
	}{{selfKey, 2}, {peerKey, 5}} {
		addr, err := mesh.MachineAddr(a.key, a.slot)
		if err != nil {
			t.Fatal(err)
		}
		want[addr] = true
	}
	for _, addr := range got {
		if !want[addr] {
			t.Errorf("resolved %s, which is not one of this app's machines", addr)
		}
	}
}

// The querying machine's app comes from its row, which is reached through the
// machine id the socket was bound with -- nothing in the packet. A guest that
// could name another app would be able to read that app's addresses.
func TestAnUngroupedMachineResolvesNothing(t *testing.T) {
	r, _, _, _ := testFleet(t)

	if got := r.Resolve(Query{MachineID: "m-loose", Name: "db"}); len(got) != 0 {
		t.Errorf("a machine in no app resolved %v", got)
	}
	// And it is itself invisible, for the same reason.
	if got := r.Resolve(Query{MachineID: "m-asker", Name: "loose"}); len(got) != 0 {
		t.Errorf("an ungrouped machine was resolvable: %v", got)
	}
}

// A suspended machine holds no slot on any host, so it has no address. An
// answer for it would point at whichever machine took that index next.
func TestASuspendedMachineIsNotResolvable(t *testing.T) {
	r, _, _, _ := testFleet(t)

	if got := r.Resolve(Query{MachineID: "m-asker", Name: "cache"}); len(got) != 0 {
		t.Errorf("a suspended machine resolved to %v", got)
	}
}

// nearest. is a hint, not a filter: it prefers the local replica and falls
// back to the rest. A strict reading would make a name stop resolving the
// moment the local one was rescued elsewhere.
func TestNearestPrefersTheLocalMachineAndFallsBack(t *testing.T) {
	r, fleet, selfKey, _ := testFleet(t)

	local, err := mesh.MachineAddr(selfKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Resolve(Query{MachineID: "m-asker", Name: "db", Mode: ModeNearest})
	if len(got) != 1 || got[0] != local {
		t.Errorf("nearest returned %v, want just the local %s", got, local)
	}

	// Take the local one away; the name must still resolve.
	remoteOnly := fakeFleet{hosts: fleet.hosts}
	for _, m := range fleet.machines {
		if m.ID != "m-db-a" {
			remoteOnly.machines = append(remoteOnly.machines, m)
		}
	}
	r2 := NewFleetResolver(remoteOnly, mesh.NewLocator("host-a", selfKey, remoteOnly))
	if got := r2.Resolve(Query{MachineID: "m-asker", Name: "db", Mode: ModeNearest}); len(got) != 1 {
		t.Errorf("nearest stopped resolving once the local machine left: %v", got)
	}
}

// A machine whose owner has no row yet cannot be placed: there is no key to
// derive its address from, and guessing would send traffic to whoever owns
// that block.
func TestAMachineOnAnUnknownHostIsNotResolvable(t *testing.T) {
	selfKey := keyFor(1)
	fleet := fakeFleet{
		hosts: []state.Host{{ID: "host-a", WGPubKey: selfKey.String()}},
		machines: []state.Machine{
			{ID: "m-asker", Name: "web", HostID: "host-a", State: "running", App: "shop", Slot: 1},
			{ID: "m-db", Name: "db", HostID: "host-unknown", State: "running", App: "shop", Slot: 4},
		},
	}
	r := NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet))

	if got := r.Resolve(Query{MachineID: "m-asker", Name: "db"}); len(got) != 0 {
		t.Errorf("resolved %v for a machine on a host with no key", got)
	}
}

// The mode prefixes are part of the name, not a separate field on the wire.
func TestInternalNameParsing(t *testing.T) {
	for _, tc := range []struct {
		qname string
		name  string
		mode  Mode
	}{
		{"db.internal.", "db", ModeAll},
		{"DB.Internal.", "db", ModeAll},
		{"rr.db.internal.", "db", ModeRoundRobin},
		{"nearest.db.internal.", "db", ModeNearest},
		{"internal.", "", ModeAll},
	} {
		name, mode := parseInternalName(tc.qname)
		if name != tc.name || mode != tc.mode {
			t.Errorf("%q parsed as (%q,%v), want (%q,%v)", tc.qname, name, mode, tc.name, tc.mode)
		}
	}
}

// serviceFleet is an app whose replicas carry GENERATED names, which is what a
// rollout actually produces: createReplica never sets Name.
//
// So nothing here is reachable by the name an application would write down.
// That is the whole point -- the only usable name is the service's.
func serviceFleet(t *testing.T) *FleetResolver {
	t.Helper()

	selfKey, peerKey := keyFor(1), keyFor(90)
	fleet := fakeFleet{
		hosts: []state.Host{
			{ID: "host-a", WGPubKey: selfKey.String()},
			{ID: "host-b", WGPubKey: peerKey.String()},
		},
		machines: []state.Machine{
			{ID: "m-asker", Name: "amber-lagoon-x9f2", HostID: "host-a",
				State: "running", App: "shop", Slot: 1, ServiceID: "svc-web", ReleaseID: "rel-1"},
			{ID: "m-db-a", Name: "quiet-harbor-4c1a", HostID: "host-a",
				State: "running", App: "shop", Slot: 2, ServiceID: "svc-db", ReleaseID: "rel-1"},
			{ID: "m-db-b", Name: "silver-meadow-77bd", HostID: "host-b",
				State: "running", App: "shop", Slot: 3, ServiceID: "svc-db", ReleaseID: "rel-1"},
			// A service named db in ANOTHER app. Must never be reachable.
			{ID: "m-db-x", Name: "hidden-valley-01ff", HostID: "host-b",
				State: "running", App: "other", Slot: 4, ServiceID: "svc-db-other", ReleaseID: "rel-9"},
		},
		services: []state.Service{
			{ID: "svc-web", Name: "web", App: "shop"},
			{ID: "svc-db", Name: "db", App: "shop"},
			{ID: "svc-db-other", Name: "db", App: "other"},
		},
	}
	return NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet))
}

// The line Phase 5 exists to prove, at the resolver.
//
// postgres://db.internal:5432 in a Dockerfile has to reach the db service. Its
// replicas are named quiet-harbor-4c1a and silver-meadow-77bd, so if this
// resolves only machine names it resolves nothing an app could have written.
func TestAServiceIsReachableByItsName(t *testing.T) {
	r := serviceFleet(t)

	addrs := r.Resolve(Query{MachineID: "m-asker", Name: "db"})
	if len(addrs) != 2 {
		t.Fatalf("db.internal returned %d addresses, want both replicas: %v", len(addrs), addrs)
	}
}

// A service name is scoped to the app exactly as a machine name is. Asked
// from the OTHER app's machine, db must mean that app's own service -- its
// single replica -- and never shop's two. Resolving from m-asker again would
// re-run TestAServiceIsReachableByItsName under a different name; this is the
// query that can actually cross the boundary.
func TestAServiceNameDoesNotCrossApps(t *testing.T) {
	r := serviceFleet(t)

	addrs := r.Resolve(Query{MachineID: "m-db-x", Name: "db"})
	if len(addrs) != 1 {
		t.Fatalf("db.internal asked from the other app returned %d addresses, "+
			"want only that app's own replica: %v", len(addrs), addrs)
	}
}

// A rollout replaces every replica and every generated name with it. The
// service name has to survive that, or it is no better than a machine name.
func TestAServiceNameSurvivesAReplicaReplacement(t *testing.T) {
	selfKey := keyFor(1)
	fleet := fakeFleet{
		hosts: []state.Host{{ID: "host-a", WGPubKey: selfKey.String()}},
		machines: []state.Machine{
			{ID: "m-asker", Name: "amber-lagoon-x9f2", HostID: "host-a",
				State: "running", App: "shop", Slot: 1, ServiceID: "svc-web", ReleaseID: "rel-2"},
			// The superseded replica is gone; a NEW one with a new name and a
			// new release id is what the service points at now.
			{ID: "m-db-new", Name: "crimson-summit-9a02", HostID: "host-a",
				State: "running", App: "shop", Slot: 7, ServiceID: "svc-db", ReleaseID: "rel-2"},
		},
		services: []state.Service{
			{ID: "svc-web", Name: "web", App: "shop"},
			{ID: "svc-db", Name: "db", App: "shop"},
		},
	}
	r := NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet))

	if addrs := r.Resolve(Query{MachineID: "m-asker", Name: "db"}); len(addrs) != 1 {
		t.Fatalf("db.internal returned %d addresses after a rollout, want the new replica: %v",
			len(addrs), addrs)
	}
}

// A machine name wins a collision, because that is what resolved before
// services had names and a name that changes meaning is worse than either
// choice. Pinned so the precedence is a decision rather than an accident.
func TestAMachineNameWinsAgainstAServiceName(t *testing.T) {
	selfKey, peerKey := keyFor(1), keyFor(90)
	fleet := fakeFleet{
		hosts: []state.Host{
			{ID: "host-a", WGPubKey: selfKey.String()},
			{ID: "host-b", WGPubKey: peerKey.String()},
		},
		machines: []state.Machine{
			{ID: "m-asker", Name: "amber-lagoon-x9f2", HostID: "host-a",
				State: "running", App: "shop", Slot: 1, ServiceID: "svc-web", ReleaseID: "rel-1"},
			// Named by hand, and colliding with the service below.
			{ID: "m-hand", Name: "db", HostID: "host-a", State: "running", App: "shop", Slot: 2},
			// Two replicas of a service that is also called db.
			{ID: "m-db-a", Name: "quiet-harbor-4c1a", HostID: "host-b",
				State: "running", App: "shop", Slot: 3, ServiceID: "svc-db", ReleaseID: "rel-1"},
			{ID: "m-db-b", Name: "silver-meadow-77bd", HostID: "host-b",
				State: "running", App: "shop", Slot: 4, ServiceID: "svc-db", ReleaseID: "rel-1"},
		},
		services: []state.Service{{ID: "svc-db", Name: "db", App: "shop"}},
	}
	r := NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet))

	addrs := r.Resolve(Query{MachineID: "m-asker", Name: "db"})
	if len(addrs) != 1 {
		t.Fatalf("db.internal returned %d addresses, want only the hand-named machine: %v",
			len(addrs), addrs)
	}
}

// A suspended replica keeps resolving, so traffic reaches the address that
// wakes it. Dropping it is what made min_machines_running: 0 unusable for a
// dependency, and the service path must not reintroduce that.
func TestASuspendedReplicaStillResolvesByServiceName(t *testing.T) {
	selfKey := keyFor(1)
	fleet := fakeFleet{
		hosts: []state.Host{{ID: "host-a", WGPubKey: selfKey.String()}},
		machines: []state.Machine{
			{ID: "m-asker", Name: "amber-lagoon-x9f2", HostID: "host-a",
				State: "running", App: "shop", Slot: 1, ServiceID: "svc-web", ReleaseID: "rel-1"},
			// Suspended, but a service replica: it kept its slot and address.
			{ID: "m-db", Name: "quiet-harbor-4c1a", HostID: "host-a",
				State: "suspended", App: "shop", Slot: 2, ServiceID: "svc-db", ReleaseID: "rel-1"},
		},
		services: []state.Service{{ID: "svc-db", Name: "db", App: "shop"}},
	}
	r := NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet))

	if addrs := r.Resolve(Query{MachineID: "m-asker", Name: "db"}); len(addrs) != 1 {
		t.Fatalf("a suspended replica did not resolve by service name: %v", addrs)
	}
}

// An ungrouped machine has no app, so it reaches no service by name either.
func TestAServiceNameIsInvisibleToAnUngroupedMachine(t *testing.T) {
	selfKey := keyFor(1)
	fleet := fakeFleet{
		hosts: []state.Host{{ID: "host-a", WGPubKey: selfKey.String()}},
		machines: []state.Machine{
			{ID: "m-loose", Name: "loose", HostID: "host-a", State: "running", Slot: 9},
			{ID: "m-db", Name: "quiet-harbor-4c1a", HostID: "host-a",
				State: "running", App: "shop", Slot: 2, ServiceID: "svc-db", ReleaseID: "rel-1"},
		},
		services: []state.Service{{ID: "svc-db", Name: "db", App: "shop"}},
	}
	r := NewFleetResolver(fleet, mesh.NewLocator("host-a", selfKey, fleet))

	if addrs := r.Resolve(Query{MachineID: "m-loose", Name: "db"}); len(addrs) != 0 {
		t.Fatalf("a machine with no app resolved a service name: %v", addrs)
	}
}
