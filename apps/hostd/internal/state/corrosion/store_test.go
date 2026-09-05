package corrosion

import (
	"context"
	"errors"
	"fmt"
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

// A service row must survive a round trip through corrosion.
//
// SQLite has no boolean, so autodeploy is an INTEGER and corrosion hands it
// back as a JSON number. Scanning it straight into a Go bool fails in the
// decoder with a message about types rather than about the column, and it
// fails on EVERY read of the table -- which reached a caller as a create that
// could not deliver an environment, naming neither services nor autodeploy.
func TestServiceRoundTripsThroughCorrosion(t *testing.T) {
	store, _ := newTestStore(t, "host-a")
	ctx := context.Background()

	want := &state.Service{
		ID: "svc-1", Name: "web", App: "shop", Replicas: 2,
		Env: `{"PORT":"8080"}`, EnvSealed: "sealed-blob",
		Repo: "vivek7405/pilots", Branch: "main", Autodeploy: true,
		CreatedAt: 1234,
	}
	if err := store.PutService(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetService(ctx, "svc-1")
	if err != nil {
		t.Fatalf("reading back a service failed: %v", err)
	}
	if got.Autodeploy != want.Autodeploy {
		t.Errorf("autodeploy = %v, want %v", got.Autodeploy, want.Autodeploy)
	}
	if got.App != want.App || got.EnvSealed != want.EnvSealed {
		t.Errorf("round trip changed the row: %+v", got)
	}
}

// hostRows for the arbiter tests: a live fleet the store can compute over.
func liveFleet(t *testing.T, agent *fakeAgent, ids ...string) {
	t.Helper()
	now := time.Now().Unix()
	for _, id := range ids {
		agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES (`+
			`'`+id+`', `+fmt.Sprint(now)+`)`)
	}
}

// A service row has exactly one writer, and it is not "whoever asked".
//
// PutMachine enforces single-writer in SQL because a machine names its host.
// A service names machines, so there is no column to guard on -- the guard is
// the deterministic arbiter every host computes identically from the live set.
// Without it, 5c's new writers (a deploy flipping release_id, an autoscaler
// writing replicas) merge under last-write-wins: no error, no conflict, half
// of each write kept.
func TestOnlyTheArbiterWritesAService(t *testing.T) {
	ctx := context.Background()

	// Find a service id and a fleet where host-a is NOT the arbiter, so the
	// test proves refusal rather than accidentally landing on the owner.
	fleet := []state.Host{{ID: "host-a"}, {ID: "host-b"}, {ID: "host-c"}}
	var owned, foreign string
	for _, id := range []string{"svc-1", "svc-2", "svc-3", "svc-4", "svc-5", "svc-6"} {
		o, _ := state.OwnerFor(id, fleet)
		if o == "host-a" && owned == "" {
			owned = id
		}
		if o != "host-a" && foreign == "" {
			foreign = id
		}
	}
	if owned == "" || foreign == "" {
		t.Fatal("could not find both an owned and a foreign service id")
	}

	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a", "host-b", "host-c")

	// The one it owns: allowed.
	if err := store.PutService(ctx, &state.Service{ID: owned, Name: "web", App: "shop"}); err != nil {
		t.Fatalf("the arbiter was refused its own service: %v", err)
	}
	// The one it does not: refused, and the row must not exist.
	err := store.PutService(ctx, &state.Service{ID: foreign, Name: "web", App: "shop"})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("writing a service this host does not arbitrate returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT count(*) FROM services WHERE id='`+foreign+`'`); got != "0" {
		t.Errorf("the refused write landed anyway: count=%s", got)
	}
}

// The volume binding names a service, not a host, so there is no column to
// guard the write on -- the guard is the same deterministic arbiter PutService
// and PutDomain use. Without it two hosts could bind one service to different
// volumes and last-write-wins would pick one, silently.
func TestOnlyTheArbiterWritesAServiceVolume(t *testing.T) {
	ctx := context.Background()

	fleet := []state.Host{{ID: "host-a"}, {ID: "host-b"}, {ID: "host-c"}}
	var owned, foreign string
	for _, id := range []string{"svc-1", "svc-2", "svc-3", "svc-4", "svc-5", "svc-6"} {
		o, _ := state.OwnerFor(id, fleet)
		if o == "host-a" && owned == "" {
			owned = id
		}
		if o != "host-a" && foreign == "" {
			foreign = id
		}
	}
	if owned == "" || foreign == "" {
		t.Fatal("could not find both an owned and a foreign service id")
	}

	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a", "host-b", "host-c")

	if err := store.PutServiceVolume(ctx,
		&state.ServiceVolume{ServiceID: owned, Ordinal: 1, VolumeID: "vol-1"}); err != nil {
		t.Fatalf("the arbiter was refused its own binding: %v", err)
	}
	err := store.PutServiceVolume(ctx,
		&state.ServiceVolume{ServiceID: foreign, Ordinal: 1, VolumeID: "vol-2"})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("binding a service this host does not arbitrate returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t,
		`SELECT count(*) FROM service_volumes WHERE service_id='`+foreign+`'`); got != "0" {
		t.Errorf("the refused write landed anyway: count=%s", got)
	}
}

// A single box has no one to race, and must not deadlock on its own arbiter.
func TestASingleHostArbitratesEverything(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a")

	for _, id := range []string{"svc-1", "svc-2", "svc-3"} {
		if err := store.PutService(context.Background(),
			&state.Service{ID: id, Name: "web", App: "shop"}); err != nil {
			t.Fatalf("single-host write of %s: %v", id, err)
		}
	}
}

// The deploy flip is compare-and-swap, so two interleaving deploys cannot
// leave the service pointing at one release while another's machines run.
func TestTheReleaseFlipIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a")

	if err := store.PutService(ctx, &state.Service{
		ID: "svc-1", Name: "web", App: "shop", ReleaseID: "rel-1",
	}); err != nil {
		t.Fatal(err)
	}

	// The winner swaps from what it saw.
	if err := store.CASServiceRelease(ctx, "svc-1", "rel-1", "rel-2"); err != nil {
		t.Fatalf("the first flip was refused: %v", err)
	}
	if got := agent.scalar(t, `SELECT release_id FROM services WHERE id='svc-1'`); got != "rel-2" {
		t.Fatalf("release_id = %q, want rel-2", got)
	}

	// The loser of the race still believes the release is rel-1, and is told no.
	err := store.CASServiceRelease(ctx, "svc-1", "rel-1", "rel-3")
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("a stale flip returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT release_id FROM services WHERE id='svc-1'`); got != "rel-2" {
		t.Errorf("the losing flip landed anyway: release_id = %q", got)
	}
}

// A release round-trips, including the mem build that makes deploys restore
// rather than boot, and the integer-bool that corrosion returns as a number.
func TestReleaseRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a")

	rel := &state.Release{
		ID: "rel-1", ServiceID: "svc-1", RootfsBuildID: "build-rootfs",
		MemBuildID: "build-mem", Healthy: true, CreatedAt: 1700000000,
	}
	if err := store.PutRelease(ctx, rel); err != nil {
		t.Fatalf("PutRelease: %v", err)
	}
	got, err := store.GetRelease(ctx, "rel-1")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if got.MemBuildID != "build-mem" || got.RootfsBuildID != "build-rootfs" {
		t.Errorf("build pair did not survive: %+v", got)
	}
	if !got.Healthy {
		t.Error("healthy came back false; the integer-bool was mis-scanned and " +
			"this release would never be a rollback target")
	}

	list, err := store.ReleasesFor(ctx, "svc-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ReleasesFor: %v (%d rows)", err, len(list))
	}
}

// Tenancy, revocations and quotas must round-trip through the agent, and the
// first two must be write-once there as well: they are the rows any host may
// write, and that is only safe while nothing can change a written value.
func TestTenancyRoundTripsAndIsWriteOnce(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	if err := store.PutTenancy(ctx, &state.Tenancy{
		ID: "m-1", OrgID: "org-1", Kind: "machine", CreatedAt: 10,
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	if err := store.PutTenancy(ctx, &state.Tenancy{
		ID: "m-1", OrgID: "org-2", Kind: "machine", CreatedAt: 20,
	}); err != nil {
		t.Fatalf("PutTenancy again: %v", err)
	}
	if got := agent.scalar(t, `SELECT org_id FROM tenancy WHERE id='m-1'`); got != "org-1" {
		t.Errorf("a second write moved the object to %q; the SQL must be ON CONFLICT DO NOTHING", got)
	}

	got, err := store.GetTenancy(ctx, "m-1")
	if err != nil {
		t.Fatalf("GetTenancy: %v", err)
	}
	if got.OrgID != "org-1" || got.Kind != "machine" {
		t.Errorf("read back %+v", got)
	}
	if _, err := store.GetTenancy(ctx, "m-absent"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("an untenanted id returned %v, want ErrNotFound", err)
	}

	all, err := store.ListTenancy(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("ListTenancy = %+v, %v", all, err)
	}
}

func TestRevocationRoundTripsAndKeepsTheKeyRow(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	if err := store.PutAPIKey(ctx, &state.APIKey{
		Hash: "cafe", OrgID: "org-1", Scopes: "admin", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	if revoked, err := store.IsRevoked(ctx, "cafe"); err != nil || revoked {
		t.Fatalf("IsRevoked before revoking = %v, %v", revoked, err)
	}
	if err := store.PutRevocation(ctx, &state.Revocation{Hash: "cafe", RevokedAt: 100}); err != nil {
		t.Fatalf("PutRevocation: %v", err)
	}
	if err := store.PutRevocation(ctx, &state.Revocation{Hash: "cafe", RevokedAt: 200}); err != nil {
		t.Fatalf("PutRevocation again: %v", err)
	}
	if got := agent.scalar(t, `SELECT revoked_at FROM api_key_revocations WHERE hash='cafe'`); got != "100" {
		t.Errorf("revoked_at moved to %q; the earliest revocation is the true one", got)
	}

	revoked, err := store.IsRevoked(ctx, "cafe")
	if err != nil || !revoked {
		t.Errorf("IsRevoked after revoking = %v, %v", revoked, err)
	}
	// The credential is killed by a row that APPEARS, never by deleting the
	// key's: a delete loses to a replica still carrying the insert.
	if got := agent.scalar(t, `SELECT count(*) FROM api_keys WHERE hash='cafe'`); got != "1" {
		t.Errorf("revoking removed the api_keys row (count=%q)", got)
	}

	keys, err := store.ListAPIKeys(ctx, "org-1")
	if err != nil || len(keys) != 1 || keys[0].Hash != "cafe" {
		t.Errorf("ListAPIKeys = %+v, %v", keys, err)
	}
	if other, _ := store.ListAPIKeys(ctx, "org-2"); len(other) != 0 {
		t.Errorf("ListAPIKeys leaked another org's keys: %+v", other)
	}
}

func TestQuotaRoundTripsThroughCorrosion(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t, "host-a")

	if _, err := store.GetQuota(ctx, "org-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("an org with no row returned %v, want ErrNotFound", err)
	}
	want := &state.Quota{OrgID: "org-1", MaxMachines: 5, MaxVCPUs: 10,
		MaxMemMiB: 2048, MaxVolumeGiB: 50, MaxBuilds: 1, UpdatedAt: 7}
	if err := store.PutQuota(ctx, want); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}
	want.MaxMachines, want.UpdatedAt = 9, 8
	if err := store.PutQuota(ctx, want); err != nil {
		t.Fatalf("PutQuota again: %v", err)
	}
	got, err := store.GetQuota(ctx, "org-1")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if *got != *want {
		t.Errorf("read back %+v, want %+v", got, want)
	}
}

// crsqlDBVersionsDDL is cr-sqlite's own, byte for byte.
//
// The fake agent is plain SQLite with no cr-sqlite extension, so a test that
// invents this table asserts the SQL and not the table -- a column renamed
// upstream would keep passing here while Version failed on every real host.
// So it is not invented: it was read off a corrosion v1.0.0 agent running this
// repo's schema.sql, with
//
//	SELECT sql FROM sqlite_master WHERE name = 'crsql_db_versions'
//
// and the production query was run against that same agent, returning 0 on a
// fresh replica and 3 after three writes. One row per actor, which is what
// makes the sum a version vector. Re-take it when CORROSION_VERSION moves in
// scripts/host-bootstrap.sh.
const crsqlDBVersionsDDL = `CREATE TABLE crsql_db_versions ` +
	`("site_id" BLOB NOT NULL PRIMARY KEY, "db_version" INTEGER NOT NULL) STRICT`

// The comparable number is the version VECTOR summed, not the scalar
// crsql_db_version(). That scalar is the local write clock: two replicas
// holding identical data but having written a different share of it carry
// different values, so comparing it across hosts says nothing.
func TestStoreVersionSumsTheVersionVector(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, crsqlDBVersionsDDL)
	agent.exec(t, `INSERT INTO crsql_db_versions VALUES (x'01', 3), (x'02', 4)`)

	v, err := store.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != 7 {
		t.Errorf("Version = %d, want 7: the sum across every actor, not one of them", v)
	}

	if !agent.asked(t, "crsql_db_versions") {
		t.Error("the version was not read from crsql_db_versions")
	}
}

// A replica that has applied nothing reads 0, not an error and not a NULL
// scan. gate.sh treats 0 as a host whose agent is not answering, so the empty
// case has to be a real zero.
func TestStoreVersionIsZeroWithNoActors(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, crsqlDBVersionsDDL)

	v, err := store.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != 0 {
		t.Errorf("Version = %d on an empty version vector, want 0", v)
	}
}

// The counterfactual for the DDL above: if a future cr-sqlite renames the
// table or the column, Version must fail loudly rather than invent a number.
// /v1/health then logs the error and reports store_version 0, which reads as
// "this replica cannot be compared" -- a wrong non-zero here would instead
// tell gate.sh the fleet is converged when nobody knows whether it is.
func TestStoreVersionErrorsRatherThanGuessingWhenTheTableIsGone(t *testing.T) {
	store, _ := newTestStore(t, "host-a")

	v, err := store.Version(context.Background())
	if err == nil {
		t.Fatalf("Version returned %d with no crsql_db_versions table, want an error", v)
	}
	if v != 0 {
		t.Errorf("Version = %d alongside an error, want 0", v)
	}
}

// A machine's cpu row says which vendor pool its memory image is in, and the
// rescue ranking reads it to decide whether a restore is even possible. There
// is no host_id column on machine_cpu to hang a WHERE on, so the guard is a
// read of the machine it describes -- and without it two hosts could name
// different pools for one image, which merges into a restore against foreign
// CPUID: the exact failure the table exists to prevent.
func TestOnlyTheOwnerWritesAMachinesCPU(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','running')`)
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-2','host-b','running')`)

	own := &state.MachineCPU{ID: "m-1", Kind: state.KindMachine, Vendor: "AuthenticAMD"}
	if err := store.PutMachineCPU(ctx, own); err != nil {
		t.Fatalf("the owner was refused its own machine's cpu row: %v", err)
	}
	if got := agent.scalar(t, `SELECT vendor FROM machine_cpu WHERE id='m-1'`); got != "AuthenticAMD" {
		t.Errorf("the owner's write did not land: vendor=%q", got)
	}

	foreign := &state.MachineCPU{ID: "m-2", Kind: state.KindMachine, Vendor: "GenuineIntel"}
	if err := store.PutMachineCPU(ctx, foreign); !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("writing another host's machine cpu row returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT count(*) FROM machine_cpu WHERE id='m-2'`); got != "0" {
		t.Errorf("the refused write landed anyway: count=%s", got)
	}
}

// A rescue writes the cpu row while the machines row still names the dead
// owner, so the same claim that authorises taking the machine authorises this.
// Liveness is re-read, exactly as ClaimMachine re-reads it: the gap between
// deciding to rescue and writing is where a host comes back.
func TestARescuerWritesTheCPURowOfADeadHostsMachine(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-dead','suspended')`)
	agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES ('host-dead', 1)`)

	row := &state.MachineCPU{ID: "m-1", Kind: state.KindMachine, Vendor: "AuthenticAMD",
		LastStart: state.StartColdBoot}
	if err := store.PutMachineCPU(ctx, row, state.WithDeadOwnerClaim("host-dead")); err != nil {
		t.Fatalf("the rescuer was refused: %v", err)
	}

	// The same claim against a host that is still heartbeating is refused.
	agent.exec(t, `UPDATE hosts SET last_seen = `+fmt.Sprint(time.Now().Unix())+` WHERE id='host-dead'`)
	err := store.PutMachineCPU(ctx, row, state.WithDeadOwnerClaim("host-dead"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("claiming from a live host returned %v, want ErrNotOwner", err)
	}
}

// A host describes its own CPU and nothing else, for the reason PutHost has
// the same rule: one writer per row means the merge has nothing to corrupt.
func TestAHostWritesOnlyItsOwnCPURow(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")

	if err := store.PutHostCPU(ctx, &state.HostCPU{HostID: "host-a", Vendor: "AuthenticAMD",
		CPUTemplate: "T2A", UpdatedAt: 5}); err != nil {
		t.Fatalf("a host was refused its own cpu row: %v", err)
	}
	if got := agent.scalar(t, `SELECT vendor FROM host_cpu WHERE host_id='host-a'`); got != "AuthenticAMD" {
		t.Errorf("the write did not land: vendor=%q", got)
	}

	err := store.PutHostCPU(ctx, &state.HostCPU{HostID: "host-b", Vendor: "GenuineIntel"})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("writing another host's cpu row returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT count(*) FROM host_cpu WHERE host_id='host-b'`); got != "0" {
		t.Errorf("the refused write landed anyway: count=%s", got)
	}
}

// A release's memory image is as vendor-locked as a machine's, and the release
// row's writer is the service arbiter, so its cpu row's writer is too.
func TestAReleaseCPURowIsWrittenByTheArbiter(t *testing.T) {
	ctx := context.Background()

	fleet := []state.Host{{ID: "host-a"}, {ID: "host-b"}, {ID: "host-c"}}
	var owned, foreign string
	for _, id := range []string{"svc-1", "svc-2", "svc-3", "svc-4", "svc-5", "svc-6"} {
		o, _ := state.OwnerFor(id, fleet)
		if o == "host-a" && owned == "" {
			owned = id
		}
		if o != "host-a" && foreign == "" {
			foreign = id
		}
	}
	if owned == "" || foreign == "" {
		t.Fatal("could not find both an owned and a foreign service id")
	}

	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a", "host-b", "host-c")
	agent.exec(t, `INSERT INTO releases (id, service_id) VALUES ('rel-own','`+owned+`')`)
	agent.exec(t, `INSERT INTO releases (id, service_id) VALUES ('rel-foreign','`+foreign+`')`)

	if err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "rel-own", Kind: state.KindRelease, Vendor: "AuthenticAMD"}); err != nil {
		t.Fatalf("the arbiter was refused its own release's cpu row: %v", err)
	}
	err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "rel-foreign", Kind: state.KindRelease, Vendor: "AuthenticAMD"})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("writing a foreign release's cpu row returned %v, want ErrNotOwner", err)
	}
}

// A checkpoint's image belongs to the pool that photographed it, and its
// writer is the owner of the machine it was taken from.
func TestACheckpointCPURowIsWrittenByItsMachinesOwner(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','running')`)
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-2','host-b','running')`)
	agent.exec(t, `INSERT INTO checkpoints (id, machine_id) VALUES ('ck-1','m-1')`)
	agent.exec(t, `INSERT INTO checkpoints (id, machine_id) VALUES ('ck-2','m-2')`)

	if err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "ck-1", Kind: state.KindCheckpoint, Vendor: "AuthenticAMD"}); err != nil {
		t.Fatalf("the machine's owner was refused its checkpoint's cpu row: %v", err)
	}
	err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "ck-2", Kind: state.KindCheckpoint, Vendor: "AuthenticAMD"})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("writing a foreign machine's checkpoint cpu row returned %v, want ErrNotOwner", err)
	}
}

// The guard finds a checkpoint's writer through checkpoints.machine_id, so the
// checkpoint row has to exist FIRST. Pinned because the natural instinct is to
// write the pool before the row that names the builds -- which is what every
// other cpu row does -- and here it refuses every checkpoint on a replicated
// store, and with it every release snapshot a deploy takes.
func TestACheckpointCPURowNeedsItsCheckpointRowFirst(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','running')`)

	err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "ck-unwritten", Kind: state.KindCheckpoint, Vendor: "AuthenticAMD"})
	if err == nil {
		t.Fatal("a cpu row for a checkpoint that does not exist yet was accepted; " +
			"machines.Checkpoint must write PutCheckpoint before PutMachineCPU")
	}
}

// The vendor reaches the ranking through ListHosts. A host with no cpu row
// reads as empty rather than dropping out of the list: it is a host that has
// not finished starting, and it is still live.
func TestListHostsJoinsTheVendor(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-a")
	liveFleet(t, agent, "host-a", "host-b")
	agent.exec(t, `INSERT INTO host_cpu (host_id, vendor) VALUES ('host-a','AuthenticAMD')`)

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("ListHosts returned %d hosts", len(hosts))
	}
	if hosts[0].Vendor != "AuthenticAMD" || hosts[1].Vendor != "" {
		t.Errorf("vendors are %q and %q, want AuthenticAMD and empty", hosts[0].Vendor, hosts[1].Vendor)
	}
}
