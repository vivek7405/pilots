package corrosion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func newTestStore(t *testing.T, hostID string) (*Store, *fakeAgent) {
	t.Helper()
	client, agent := newFakeAgent(t)
	return NewStore(client, hostID), agent
}

func machine(id, hostID, machineState string) *state.Machine {
	return &state.Machine{
		ID: id, Name: id, HostID: hostID, State: machineState,
		VCPUs: 1, MemMiB: 512, Domain: id + ".pilotrun.app",
		UpdatedAt: time.Now().Unix(),
	}
}

// A host writes only rows describing its own machines. Corrosion accepts a
// violating write and merges it -- no error, no conflict, nothing logged --
// and the damage surfaces later as a row two hosts both believe they own. So
// the rule has to be enforced where the write happens.
func TestPutMachineRefusesAnotherHostsMachine(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)

	err := store.PutMachine(context.Background(), machine("m-1", "host-b", "stopped"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("PutMachine on another host's machine returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT state FROM machines WHERE id='m-1'`); got != "running" {
		t.Errorf("the row was changed anyway: state=%q", got)
	}
}

func TestPutMachineWritesItsOwn(t *testing.T) {
	store, agent := newTestStore(t, "host-a")

	if err := store.PutMachine(context.Background(), machine("m-1", "host-a", "running")); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if got := agent.scalar(t, `SELECT state FROM machines WHERE id='m-1'`); got != "running" {
		t.Errorf("state = %q", got)
	}

	got, err := store.GetMachine(context.Background(), "m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.HostID != "host-a" || got.Domain != "m-1.pilotrun.app" {
		t.Errorf("read back %+v", got)
	}
}

// The resurrected-owner case, and the single most important property here.
//
// A host is partitioned, its machine is rescued elsewhere, and it comes back
// still believing it owns the machine. Everything it writes from then on --
// state, build ids, activity -- must be unable to take ownership back, because
// merges are per column and a re-asserted host_id would win purely by being
// later.
func TestPutMachineCannotTakeOwnershipBack(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	// host-a's machine, since rescued by host-b.
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)

	// host-a comes back and writes the row as it remembers it.
	stale := machine("m-1", "host-a", "running")
	err := store.PutMachine(ctx, stale)

	if !errors.Is(err, state.ErrNotOwner) {
		t.Errorf("the stale owner's write was accepted: %v", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-b" {
		t.Fatalf("ownership went back to the stale owner: host_id=%q", got)
	}
}

// Even an authorised write must not carry host_id: the exceptions exist to let
// a name be allocated or a dead host's machine be claimed, not to let any
// write move ownership as a side effect. Ownership moves only through
// ClaimMachine.
func TestPutMachineNeverUpdatesOwnership(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)

	if err := store.PutMachine(ctx, machine("m-1", "host-a", "stopped"),
		state.WithNameAllocation()); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-b" {
		t.Errorf("host_id = %q; an ordinary write moved ownership", got)
	}
	if got := agent.scalar(t, `SELECT state FROM machines WHERE id='m-1'`); got != "stopped" {
		t.Errorf("state = %q; the authorised write did not land", got)
	}
}

// A claim is only legitimate while the owner is actually gone. The gap between
// a rescue loop deciding and writing is exactly where a host comes back, so
// liveness is re-read at the write rather than trusted from the caller.
func TestClaimRefusedWhileTheOwnerIsStillAlive(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)
	agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES ('host-b', ?)`, time.Now().Unix())

	err := store.ClaimMachine(ctx, "m-1", "host-a", "creating",
		state.WithDeadOwnerClaim("host-b"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("claimed a machine from a live host: %v", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-b" {
		t.Errorf("host_id = %q, want it untouched", got)
	}
}

func TestClaimTakesAMachineFromADeadHost(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)
	agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES ('host-b', ?)`,
		time.Now().Add(-5*time.Minute).Unix())

	if err := store.ClaimMachine(ctx, "m-1", "host-a", "creating",
		state.WithDeadOwnerClaim("host-b")); err != nil {
		t.Fatalf("ClaimMachine: %v", err)
	}

	// Owner and state must have moved TOGETHER. Split across two writes, the
	// merge can leave the row owned by the rescuer while still reporting what
	// the dead owner last said -- a machine claimed by someone and running
	// nowhere.
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-a" {
		t.Errorf("host_id = %q, want host-a", got)
	}
	if got := agent.scalar(t, `SELECT state FROM machines WHERE id='m-1'`); got != "creating" {
		t.Errorf("state = %q, want creating", got)
	}
}

// A claim without the option is a programming error, not a policy decision.
func TestClaimRequiresTheExplicitOption(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)

	if err := store.ClaimMachine(context.Background(), "m-1", "host-a", "creating"); err == nil {
		t.Error("a claim with no WithDeadOwnerClaim was accepted")
	}
}

// Two survivors racing for the same machine: only one can win, and the loser
// must learn it lost rather than proceeding to restore a machine someone else
// is already restoring.
func TestClaimIsLostCleanlyWhenAnotherHostGetsThereFirst(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-dead','running')`)
	agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES ('host-dead', ?)`,
		time.Now().Add(-5*time.Minute).Unix())

	// host-c claimed it a moment ago.
	agent.exec(t, `UPDATE machines SET host_id='host-c' WHERE id='m-1'`)

	err := store.ClaimMachine(ctx, "m-1", "host-a", "creating",
		state.WithDeadOwnerClaim("host-dead"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("the losing claim returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-c" {
		t.Errorf("host_id = %q; the loser overwrote the winner", got)
	}
}

// A delete racing an update loses through the merge and the row comes back, so
// destruction is a state rather than a DELETE -- invisible to every read, and
// collected later by the reaper.
func TestDeleteTombstonesRatherThanDeleting(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	if err := store.PutMachine(ctx, machine("m-1", "host-a", "running")); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := store.DeleteMachine(ctx, "m-1"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}

	if got := agent.scalar(t, `SELECT state FROM machines WHERE id='m-1'`); got != state.StateDestroyed {
		t.Errorf("row state = %q, want the tombstone", got)
	}
	if _, err := store.GetMachine(ctx, "m-1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a destroyed machine is still readable: %v", err)
	}
	all, err := store.ListMachines(ctx)
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	for _, m := range all {
		if m.ID == "m-1" {
			t.Error("a destroyed machine is still listed")
		}
	}
}

func TestPutHostRefusesAnotherHostsRow(t *testing.T) {
	store, _ := newTestStore(t, "host-a")

	err := store.PutHost(context.Background(), &state.Host{ID: "host-b", LastSeen: time.Now().Unix()})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Errorf("wrote another host's row: %v", err)
	}
}

func TestPutHostRoundTripsTheMeshIdentity(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t, "host-a")

	want := &state.Host{
		ID: "host-a", WGAddr: "fdcc::1", WGPubKey: "abc123",
		PublicIP: "203.0.113.7", CPUFree: 8, MemFreeMiB: 4096,
		LastSeen: time.Now().Unix(),
	}
	if err := store.PutHost(ctx, want); err != nil {
		t.Fatalf("PutHost: %v", err)
	}

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts", len(hosts))
	}
	if hosts[0].WGAddr != want.WGAddr || hosts[0].WGPubKey != want.WGPubKey {
		t.Errorf("mesh identity did not round-trip: %+v", hosts[0])
	}
}

func TestCheckpointsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t, "host-a")

	want := &state.Checkpoint{
		ID: "ck-1", MachineID: "m-1", Seq: 1, Comment: "v1",
		MemBuildID: "mb-1", RootfsBuildID: "rb-1", Durable: true,
		CreatedAt: time.Now().Unix(),
	}
	if err := store.PutCheckpoint(ctx, want); err != nil {
		t.Fatalf("PutCheckpoint: %v", err)
	}

	got, err := store.ListCheckpoints(ctx, "m-1")
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ck-1" || !got[0].Durable || got[0].MemBuildID != "mb-1" {
		t.Errorf("read back %+v", got)
	}
}

func TestAPIKeysRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t, "host-a")

	if err := store.PutAPIKey(ctx, &state.APIKey{
		Hash: "deadbeef", OrgID: "org-1", Scopes: "machines",
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	got, err := store.GetAPIKeyByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if got.OrgID != "org-1" {
		t.Errorf("read back %+v", got)
	}
	if _, err := store.GetAPIKeyByHash(ctx, "absent"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("an absent key returned %v, want ErrNotFound", err)
	}
}
