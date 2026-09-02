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
}

func (f fakeFleet) Machines() []state.Machine { return f.machines }
func (f fakeFleet) Hosts() []state.Host       { return f.hosts }

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
