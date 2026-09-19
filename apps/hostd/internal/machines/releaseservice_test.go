package machines

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// recordingHandoffs is a HandoffNotifier that remembers what it was asked.
type recordingHandoffs struct{ released []string }

func (r *recordingHandoffs) Offer(context.Context, string, string, string) error { return nil }

func (r *recordingHandoffs) ReleaseService(_ context.Context, hostID, serviceID string) error {
	r.released = append(r.released, hostID+"/"+serviceID)
	return nil
}

// liveHosts writes hosts that heartbeated just now, and returns them in the
// shape OwnerFor ranks.
func liveHosts(t *testing.T, ctx context.Context, store state.Store, ids ...string) []state.Host {
	t.Helper()
	live := make([]state.Host, 0, len(ids))
	for _, id := range ids {
		h := &state.Host{ID: id, LastSeen: time.Now().Unix()}
		if err := store.PutHost(ctx, h); err != nil {
			t.Fatalf("PutHost %s: %v", id, err)
		}
		live = append(live, *h)
	}
	return live
}

// serviceArbitratedBy writes a service whose arbiter is want.
//
// Minted rather than provisioned: provisionService mints ids this host
// arbitrates (state.NewOwnedID), so a service provisioned here is always
// host-a's by construction. On a fleet the case under test arises anyway,
// because a service's REPLICAS are placed by ranking on whichever host has
// room, and it is the host holding the last replica -- not the arbiter --
// that runs releaseService. NewOwnedID takes the host to mint for, which is
// how a test reaches the other side deterministically.
func serviceArbitratedBy(t *testing.T, ctx context.Context, store state.Store, live []state.Host, want string) string {
	t.Helper()
	id := state.NewOwnedID("svc_", want, live)
	if owner, ok := state.OwnerFor(id, live); !ok || owner != want {
		t.Fatalf("NewOwnedID minted %s for %s, but OwnerFor names %q", id, want, owner)
	}
	if err := store.PutService(ctx, &state.Service{ID: id, Name: "web", App: "shop", Replicas: 1}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	return id
}

// lastReplicaOf writes one machine of the service and removes it, which is
// the state releaseService is called in: the last replica is gone.
func lastReplicaOf(t *testing.T, ctx context.Context, store state.Store, svcID string) *state.Machine {
	t.Helper()
	row := &state.Machine{ID: "m-1", HostID: "host-a", ServiceID: svcID, State: "running"}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := store.DeleteMachine(ctx, row.ID); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	return row
}

// The rows only a service's ARBITER may write are released THERE. On a fleet
// the arbiter is a hash over the live hosts, unrelated to which host held the
// last replica, and the store refuses those deletes from anyone else. So
// releasing them here failed on every host but one and, by returning early,
// left the service row itself gossiping forever. Now the host that held the
// replica asks the arbiter for that half and removes only what no writer rule
// covers, the service row included.
func TestReleasingAServiceSendsTheArbitersRowsToTheArbiter(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)
	live := liveHosts(t, ctx, store, "host-a", "host-b")
	peers := &recordingHandoffs{}
	m.opts.Handoffs = peers

	svcID := serviceArbitratedBy(t, ctx, store, live, "host-b")
	if err := store.PutRelease(ctx, &state.Release{ID: "rel-1", ServiceID: svcID}); err != nil {
		t.Fatalf("PutRelease: %v", err)
	}
	row := lastReplicaOf(t, ctx, store, svcID)

	if err := m.releaseService(ctx, row); err != nil {
		t.Fatalf("releaseService: %v", err)
	}
	if len(peers.released) != 1 || peers.released[0] != "host-b/"+svcID {
		t.Errorf("released = %v, want the arbiter host-b asked for %s", peers.released, svcID)
	}
	// The arbiter's rows were left for the arbiter, not deleted from here.
	if rels, _ := store.ReleasesFor(ctx, svcID); len(rels) != 1 {
		t.Errorf("the releases were deleted here rather than left for the arbiter: %v", rels)
	}
	// And the rows no writer rule covers still went, the service row last.
	if _, err := store.GetService(ctx, svcID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the service row survived its last machine: err=%v", err)
	}
}

// When this host IS the arbiter the rows go here and no peer is asked. The
// counterfactual for the test above: same fleet, a service that hashes the
// other way.
func TestReleasingAServiceDeletesTheArbitersRowsWhenItIsTheArbiter(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)
	live := liveHosts(t, ctx, store, "host-a", "host-b")
	peers := &recordingHandoffs{}
	m.opts.Handoffs = peers

	svcID := serviceArbitratedBy(t, ctx, store, live, "host-a")
	if err := store.PutRelease(ctx, &state.Release{ID: "rel-1", ServiceID: svcID}); err != nil {
		t.Fatalf("PutRelease: %v", err)
	}
	row := lastReplicaOf(t, ctx, store, svcID)

	if err := m.releaseService(ctx, row); err != nil {
		t.Fatalf("releaseService: %v", err)
	}
	if len(peers.released) != 0 {
		t.Errorf("asked a peer although this host is the arbiter: %v", peers.released)
	}
	if rels, _ := store.ReleasesFor(ctx, svcID); len(rels) != 0 {
		t.Errorf("the arbiter's rows survived on the arbiter: %v", rels)
	}
	if _, err := store.GetService(ctx, svcID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the service row survived: err=%v", err)
	}
}
