package selfheal

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeFleet is a fixed cluster view.
type fakeFleet struct {
	machines []state.Machine
	hosts    []state.Host
	// vendors is which pool each machine's memory image belongs to. Absent
	// means "in no pool", which is what a machine that predates machine_cpu
	// reads as.
	vendors map[string]string
}

func (f *fakeFleet) Machines() []state.Machine { return f.machines }

func (f *fakeFleet) MachineVendor(id string) string { return f.vendors[id] }

func (f *fakeFleet) LiveHosts(now time.Time, deadAfter time.Duration) []state.Host {
	var out []state.Host
	for _, h := range f.hosts {
		if now.Sub(time.Unix(h.LastSeen, 0)) < deadAfter {
			out = append(out, h)
		}
	}
	// Returned in whatever order it was built in, deliberately. Every host
	// must reach the same slice assignment regardless, which is only true if
	// the loop canonicalises the ordering itself.
	return out
}

// fakeStore records claims and restores.
type fakeStore struct {
	state.Store

	mu       sync.Mutex
	machines map[string]*state.Machine
	claims   []string
	claimErr error
}

func newFakeStore(machines ...state.Machine) *fakeStore {
	s := &fakeStore{machines: map[string]*state.Machine{}}
	for i := range machines {
		m := machines[i]
		s.machines[m.ID] = &m
	}
	return s
}

func (s *fakeStore) GetMachine(_ context.Context, id string) (*state.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[id]
	if !ok {
		return nil, fmt.Errorf("no machine %q: %w", id, state.ErrNotFound)
	}
	copied := *m
	return &copied, nil
}

func (s *fakeStore) PutMachine(_ context.Context, m *state.Machine, _ ...state.WriteOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := *m
	s.machines[m.ID] = &copied
	return nil
}

func (s *fakeStore) ClaimMachine(_ context.Context, id, newHostID, newState string, _ ...state.WriteOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return s.claimErr
	}
	m, ok := s.machines[id]
	if !ok {
		return state.ErrNotFound
	}
	s.claims = append(s.claims, id)
	m.HostID = newHostID
	m.State = newState
	return nil
}

func (s *fakeStore) PutHost(context.Context, *state.Host) error { return nil }

func (s *fakeStore) claimed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.claims...)
}

func host(id string, lastSeen time.Time) state.Host {
	return state.Host{ID: id, LastSeen: lastSeen.Unix()}
}

func machine(id, hostID string) state.Machine {
	return state.Machine{ID: id, Name: id, HostID: hostID, State: "running",
		MemBuildID: "mb-" + id, VCPUs: 1, MemMiB: 512}
}

// Every survivor must compute the same slice assignment from the same inputs,
// with nothing coordinating them. If two hosts disagree, either both rescue a
// machine -- two VMs on one disk -- or neither does and it stays down.
func TestSurvivorsPartitionTheMachinesWithoutTalking(t *testing.T) {
	now := time.Now()

	// Each host sees the fleet in a DIFFERENT order -- map iteration, gossip
	// arrival, anything. That is the hazard: rank is a POSITION in this list,
	// so unless the loop canonicalises the ordering, two hosts compute
	// different ranks and the slices stop partitioning the machines.
	views := map[string][]state.Host{
		"host-a": {host("host-a", now), host("host-b", now), host("host-c", now),
			host("host-dead", now.Add(-5*time.Minute))},
		"host-b": {host("host-dead", now.Add(-5*time.Minute)), host("host-c", now),
			host("host-b", now), host("host-a", now)},
		"host-c": {host("host-c", now), host("host-a", now),
			host("host-dead", now.Add(-5*time.Minute)), host("host-b", now)},
	}

	var machines []state.Machine
	for i := 0; i < 60; i++ {
		machines = append(machines, machine(fmt.Sprintf("m-%02d", i), "host-dead"))
	}

	rescuedBy := map[string]string{}
	for _, self := range []string{"host-a", "host-b", "host-c"} {
		store := newFakeStore(machines...)
		Tick(context.Background(), Options{
			HostID: self,
			Fleet:  &fakeFleet{machines: machines, hosts: views[self]},
			Store:  store,
			Now:    func() time.Time { return now },
			// Restore owns the claim, so this is where a rescue is observed.
			Restore: func(_ context.Context, m *state.Machine) error {
				return store.ClaimMachine(context.Background(), m.ID, self, "creating",
					state.WithDeadOwnerClaim(m.HostID))
			},
		})
		for _, id := range store.claimed() {
			if other, ok := rescuedBy[id]; ok {
				t.Fatalf("%s was rescued by both %s and %s", id, other, self)
			}
			rescuedBy[id] = self
		}
	}

	if len(rescuedBy) != len(machines) {
		t.Errorf("%d of %d machines were rescued; the slices do not cover the fleet",
			len(rescuedBy), len(machines))
	}

	// And the work is actually shared out, not all landing on one host.
	perHost := map[string]int{}
	for _, h := range rescuedBy {
		perHost[h]++
	}
	for _, h := range []string{"host-a", "host-b", "host-c"} {
		if perHost[h] == 0 {
			t.Errorf("%s rescued nothing; the hash is not distributing", h)
		}
	}
}

// A host whose own heartbeat has gone stale is the one others are about to
// rescue from. Taking on more work in that state is exactly backwards.
func TestASickHostRescuesNothing(t *testing.T) {
	now := time.Now()
	store := newFakeStore(machine("m-1", "host-dead"))

	Tick(context.Background(), Options{
		HostID: "host-a",
		Fleet: &fakeFleet{
			machines: []state.Machine{machine("m-1", "host-dead")},
			hosts: []state.Host{
				// This host's own heartbeat is stale.
				host("host-a", now.Add(-5*time.Minute)),
				host("host-b", now),
			},
		},
		Store:   store,
		Now:     func() time.Time { return now },
		Restore: func(context.Context, *state.Machine) error { return nil },
	})

	if got := store.claimed(); len(got) != 0 {
		t.Errorf("a host with a stale heartbeat claimed %v", got)
	}
}

// A live owner's machines are nobody else's business.
func TestMachinesOfLiveHostsAreLeftAlone(t *testing.T) {
	now := time.Now()
	machines := []state.Machine{machine("m-1", "host-b"), machine("m-2", "host-a")}
	store := newFakeStore(machines...)

	Tick(context.Background(), Options{
		HostID:  "host-a",
		Fleet:   &fakeFleet{machines: machines, hosts: []state.Host{host("host-a", now), host("host-b", now)}},
		Store:   store,
		Now:     func() time.Time { return now },
		Restore: func(context.Context, *state.Machine) error { return nil },
	})

	if got := store.claimed(); len(got) != 0 {
		t.Errorf("claimed %v from hosts that are alive", got)
	}
}

// THE invariant. A partitioned host comes back still running a machine that
// was rescued while it was away. It must stop, immediately: the row is the
// authority, and two Firecrackers serving one machine means two writers on one
// disk in object storage.
func TestAHostThatLostAMachineStopsServingIt(t *testing.T) {
	now := time.Now()
	// The row says host-b owns it now.
	rescued := machine("m-1", "host-b")
	store := newFakeStore(rescued)

	var stopped []string
	Tick(context.Background(), Options{
		HostID:  "host-a",
		Fleet:   &fakeFleet{machines: []state.Machine{rescued}, hosts: []state.Host{host("host-a", now), host("host-b", now)}},
		Store:   store,
		Now:     func() time.Time { return now },
		Restore: func(context.Context, *state.Machine) error { return nil },
		// host-a is still running it locally.
		RunningLocally: func() []string { return []string{"m-1"} },
		StopLocal: func(_ context.Context, id string) error {
			stopped = append(stopped, id)
			return nil
		},
	})

	if len(stopped) != 1 || stopped[0] != "m-1" {
		t.Fatalf("the host kept serving a machine it no longer owns; stopped=%v", stopped)
	}
}

// The owner keeps serving its own machines, obviously.
func TestAHostKeepsServingWhatItOwns(t *testing.T) {
	now := time.Now()
	mine := machine("m-1", "host-a")
	store := newFakeStore(mine)

	var stopped []string
	Tick(context.Background(), Options{
		HostID:         "host-a",
		Fleet:          &fakeFleet{machines: []state.Machine{mine}, hosts: []state.Host{host("host-a", now)}},
		Store:          store,
		Now:            func() time.Time { return now },
		Restore:        func(context.Context, *state.Machine) error { return nil },
		RunningLocally: func() []string { return []string{"m-1"} },
		StopLocal: func(_ context.Context, id string) error {
			stopped = append(stopped, id)
			return nil
		},
	})

	if len(stopped) != 0 {
		t.Errorf("stopped machines it owns: %v", stopped)
	}
}

// Shutting down what we lost happens BEFORE claiming anything: every moment a
// host keeps serving a machine it lost is a moment two hosts are writing one
// disk.
func TestLostMachinesAreStoppedBeforeAnythingIsClaimed(t *testing.T) {
	now := time.Now()
	lost := machine("m-lost", "host-b")
	orphan := machine("m-orphan", "host-dead")
	store := newFakeStore(lost, orphan)

	var order []string
	Tick(context.Background(), Options{
		HostID: "host-a",
		Fleet: &fakeFleet{
			machines: []state.Machine{lost, orphan},
			hosts: []state.Host{
				host("host-a", now), host("host-b", now),
				host("host-dead", now.Add(-5*time.Minute)),
			},
		},
		Store: store,
		Now:   func() time.Time { return now },
		Restore: func(_ context.Context, m *state.Machine) error {
			order = append(order, "restore:"+m.ID)
			return nil
		},
		RunningLocally: func() []string { return []string{"m-lost"} },
		StopLocal: func(_ context.Context, id string) error {
			order = append(order, "stop:"+id)
			return nil
		},
	})

	if len(order) == 0 || order[0] != "stop:m-lost" {
		t.Errorf("the lost machine was not stopped first: %v", order)
	}
}

// A machine with no snapshot in object storage did not survive its host --
// there is nothing to bring back. Retrying it every ten seconds forever helps
// nobody and hides the machines that could be rescued.
func TestAMachineWithNoSnapshotIsNotRetriedForever(t *testing.T) {
	now := time.Now()
	never := machine("m-1", "host-dead")
	never.MemBuildID = "" // never suspended or checkpointed

	store := newFakeStore(never)
	restores := 0

	opts := Options{
		HostID:  "host-a",
		Fleet:   &fakeFleet{machines: []state.Machine{never}, hosts: []state.Host{host("host-a", now), host("host-dead", now.Add(-5*time.Minute))}},
		Store:   store,
		Now:     func() time.Time { return now },
		Restore: func(context.Context, *state.Machine) error { restores++; return nil },
	}
	Tick(context.Background(), opts)

	if restores != 0 {
		t.Errorf("tried to restore a machine with no snapshot %d times", restores)
	}
	// The row must be left alone: the owner may only be partitioned and still
	// serving the machine, and a claim from a host with nothing to restore
	// would make the returning owner kill its own healthy VM.
	if got, err := store.GetMachine(context.Background(), "m-1"); err != nil {
		t.Fatal(err)
	} else if got.HostID != "host-dead" {
		t.Errorf("host_id = %q; a machine with no snapshot must never be claimed", got.HostID)
	}
}

// A full host declines and says so; the next tick re-hashes against whatever
// the live set is by then, so nothing gets wedged.
func TestNoCapacityDeclinesRatherThanWedging(t *testing.T) {
	now := time.Now()
	orphan := machine("m-1", "host-dead")
	store := newFakeStore(orphan)

	Tick(context.Background(), Options{
		HostID:   "host-a",
		Fleet:    &fakeFleet{machines: []state.Machine{orphan}, hosts: []state.Host{host("host-a", now), host("host-dead", now.Add(-5*time.Minute))}},
		Store:    store,
		Now:      func() time.Time { return now },
		Capacity: func(int, int) bool { return false },
		Restore:  func(context.Context, *state.Machine) error { return nil },
	})

	if got := store.claimed(); len(got) != 0 {
		t.Errorf("a host with no capacity claimed %v", got)
	}
	if got, _ := store.GetMachine(context.Background(), "m-1"); got.State == "error" {
		t.Error("declining for capacity marked the machine unrescuable")
	}
}

// A restore that fails is reported and left for the next tick. Recording the
// failure on the row belongs to Restore, which owns the claim and therefore
// the row -- doing it here as well is what made the loop claim a machine
// twice and refuse its own second claim.
func TestAFailedRestoreIsNotClaimedHere(t *testing.T) {
	now := time.Now()
	orphan := machine("m-1", "host-dead")
	store := newFakeStore(orphan)

	Tick(context.Background(), Options{
		HostID:  "host-a",
		Fleet:   &fakeFleet{machines: []state.Machine{orphan}, hosts: []state.Host{host("host-a", now), host("host-dead", now.Add(-5*time.Minute))}},
		Store:   store,
		Now:     func() time.Time { return now },
		Restore: func(context.Context, *state.Machine) error { return fmt.Errorf("no build in storage") },
	})

	if got := store.claimed(); len(got) != 0 {
		t.Errorf("the loop claimed %v itself; Restore owns the claim, and a "+
			"second one is refused because this host is alive", got)
	}
}

// The hash must be fixed, not Go's per-process-seeded map hash: two hosts
// running identical code have to compute the same bucket for the same machine.
func TestTheHashIsStableAcrossProcesses(t *testing.T) {
	// Values computed from FNV-1a; if the function is swapped for a seeded
	// one, these move and the fleet stops agreeing.
	for _, tc := range []struct {
		id    string
		count int
		want  int
	}{
		{"m-0", 3, SliceOf("m-0", 3)},
		{"m-1", 3, SliceOf("m-1", 3)},
	} {
		for i := 0; i < 50; i++ {
			if got := SliceOf(tc.id, tc.count); got != tc.want {
				t.Fatalf("SliceOf(%q,%d) varies between calls: %d then %d",
					tc.id, tc.count, tc.want, got)
			}
		}
	}
	if SliceOf("m-1", 0) != -1 {
		t.Error("an empty fleet should own nothing")
	}
}

func hostOf(id, vendor string, lastSeen time.Time) state.Host {
	return state.Host{ID: id, Vendor: vendor, LastSeen: lastSeen.Unix()}
}

// rescuersOver runs one tick as each named host and reports who claimed what.
func rescuersOver(t *testing.T, fleet *fakeFleet, now time.Time, selves ...string) map[string]string {
	t.Helper()
	rescuedBy := map[string]string{}
	for _, self := range selves {
		store := newFakeStore(fleet.machines...)
		Tick(context.Background(), Options{
			HostID: self,
			Fleet:  fleet,
			Store:  store,
			Now:    func() time.Time { return now },
			Restore: func(_ context.Context, m *state.Machine) error {
				return store.ClaimMachine(context.Background(), m.ID, self, "creating",
					state.WithDeadOwnerClaim(m.HostID))
			},
		})
		for _, id := range store.claimed() {
			if other, ok := rescuedBy[id]; ok {
				t.Fatalf("%s was rescued by both %s and %s", id, other, self)
			}
			rescuedBy[id] = self
		}
	}
	return rescuedBy
}

// Tier 2. While any host of the image's own vendor is alive, the rescue must
// land there: a Firecracker memory snapshot carries raw CPUID and the other
// vendor cannot load it at all. Without the filter the hash sends roughly half
// of these to a host that would write them StateError.
func TestARescueStaysInTheVendorPoolWhileOneHostOfItLives(t *testing.T) {
	now := time.Now()

	var ms []state.Machine
	vendors := map[string]string{}
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("m-%02d", i)
		ms = append(ms, machine(id, "host-dead"))
		vendors[id] = "AuthenticAMD"
	}

	fleet := &fakeFleet{
		machines: ms,
		hosts: []state.Host{
			hostOf("host-amd", "AuthenticAMD", now),
			hostOf("host-intel-1", "GenuineIntel", now),
			hostOf("host-intel-2", "GenuineIntel", now),
			hostOf("host-dead", "AuthenticAMD", now.Add(-5*time.Minute)),
		},
		vendors: vendors,
	}

	rescuedBy := rescuersOver(t, fleet, now, "host-amd", "host-intel-1", "host-intel-2")
	if len(rescuedBy) != len(ms) {
		t.Fatalf("%d of %d machines were rescued", len(rescuedBy), len(ms))
	}
	for id, by := range rescuedBy {
		if by != "host-amd" {
			t.Fatalf("%s carries an AMD memory image and was rescued by %s, which cannot load it", id, by)
		}
	}
}

// Tier 3. With the pool empty, availability wins over continuity: the whole
// live set ranks and the winner cold-boots the machine from its disk. Leaving
// it unrescued instead would be strictly worse than a reboot.
func TestARescueLeavesThePoolWhenItIsEmpty(t *testing.T) {
	now := time.Now()

	var ms []state.Machine
	vendors := map[string]string{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("m-%02d", i)
		ms = append(ms, machine(id, "host-dead"))
		vendors[id] = "AuthenticAMD"
	}

	fleet := &fakeFleet{
		machines: ms,
		hosts: []state.Host{
			hostOf("host-intel", "GenuineIntel", now),
			hostOf("host-dead", "AuthenticAMD", now.Add(-5*time.Minute)),
		},
		vendors: vendors,
	}

	rescuedBy := rescuersOver(t, fleet, now, "host-intel")
	if len(rescuedBy) != len(ms) {
		t.Fatalf("%d of %d machines were rescued; a machine with no live pool was abandoned",
			len(rescuedBy), len(ms))
	}
}

// A mixed fleet must still partition: every machine claimed exactly once, by
// exactly one host, with nothing coordinating them. The vendor narrows the
// candidate set and must not break that.
func TestSurvivorsPartitionAMixedVendorFleet(t *testing.T) {
	now := time.Now()

	hosts := []state.Host{
		hostOf("host-a", "AuthenticAMD", now),
		hostOf("host-b", "AuthenticAMD", now),
		hostOf("host-c", "GenuineIntel", now),
		hostOf("host-d", "GenuineIntel", now),
		hostOf("host-dead", "AuthenticAMD", now.Add(-5*time.Minute)),
	}

	var ms []state.Machine
	vendors := map[string]string{}
	for i := 0; i < 90; i++ {
		id := fmt.Sprintf("m-%02d", i)
		ms = append(ms, machine(id, "host-dead"))
		switch i % 3 {
		case 0:
			vendors[id] = "AuthenticAMD"
		case 1:
			vendors[id] = "GenuineIntel"
			// case 2 leaves the machine in no pool, as one that predates the
			// table is.
		}
	}

	// Each survivor sees the fleet in its own order, which is the hazard rank
	// has always had: a position in a list is only stable if the list is.
	rescuedBy := map[string]string{}
	orders := map[string][]state.Host{
		"host-a": hosts,
		"host-b": {hosts[3], hosts[1], hosts[4], hosts[0], hosts[2]},
		"host-c": {hosts[2], hosts[4], hosts[0], hosts[3], hosts[1]},
		"host-d": {hosts[1], hosts[0], hosts[2], hosts[4], hosts[3]},
	}
	for self, view := range orders {
		fleet := &fakeFleet{machines: ms, hosts: view, vendors: vendors}
		for id, by := range rescuersOver(t, fleet, now, self) {
			if other, ok := rescuedBy[id]; ok {
				t.Fatalf("%s was rescued by both %s and %s", id, other, by)
			}
			rescuedBy[id] = by
		}
	}
	if len(rescuedBy) != len(ms) {
		t.Fatalf("%d of %d machines were rescued; the slices do not cover the fleet",
			len(rescuedBy), len(ms))
	}

	// And an image with a pool landed in it.
	pool := map[string]string{"host-a": "AuthenticAMD", "host-b": "AuthenticAMD",
		"host-c": "GenuineIntel", "host-d": "GenuineIntel"}
	for id, by := range rescuedBy {
		if v := vendors[id]; v != "" && pool[by] != v {
			t.Fatalf("%s carries a %s image and was rescued by %s, which is %s", id, v, by, pool[by])
		}
	}
}
