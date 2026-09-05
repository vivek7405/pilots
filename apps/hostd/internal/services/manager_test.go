package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeMachines is a machine layer that cannot boot, which is the point: the
// rollout's job is ordering and gating, and both are testable without a
// Firecracker anywhere near them.
type fakeMachines struct {
	mu       sync.Mutex
	store    state.Store
	next     int
	healthy  map[string]bool // machine -> passes its probe
	events   []string        // ordered log of what happened, for assertions
	failNth  int             // fail the Nth create (1-based), 0 = never
	creates  int
	noSnap   bool // Checkpoint produces no memory build
	suspends []string
	touches  []string
	// healthyAfterRedeploy is what a redeployed machine's probe answers. False
	// is how a test drives a failed gate onto the recovery path.
	healthyAfterRedeploy bool
}

func newFakeMachines(store state.Store) *fakeMachines {
	return &fakeMachines{store: store, healthy: map[string]bool{}, healthyAfterRedeploy: true}
}

func (f *fakeMachines) log(format string, a ...any) {
	f.events = append(f.events, fmt.Sprintf(format, a...))
}

func (f *fakeMachines) Create(ctx context.Context, req api.CreateMachineRequest) (*state.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.failNth != 0 && f.creates == f.failNth {
		f.log("create-failed")
		return nil, errors.New("boom")
	}
	f.next++
	id := fmt.Sprintf("m-%d", f.next)

	how := "boot"
	if req.MemBuildID != "" {
		how = "restore"
	}
	// A fourth segment, so the existing readers of the third keep working.
	f.log("create:%s:%s:vol=%s", id, how, req.Volume)

	// The fake enforces the claim the real engine does: a volume is held by
	// one machine, so a create naming one another live machine already holds
	// is refused here rather than passing in a test and failing on a host.
	if req.Volume != "" {
		all, err := f.store.ListMachines(ctx)
		if err != nil {
			return nil, err
		}
		for _, other := range all {
			if other.State != state.StateDestroyed && other.VolumeID == req.Volume {
				return nil, fmt.Errorf("volume %s is held by %s", req.Volume, other.ID)
			}
		}
	}

	row := &state.Machine{
		ID: id, Name: id, HostID: "host-a", State: "running",
		ServiceID: req.Service, ReleaseID: req.Release,
		VolumeID:  req.Volume,
		KindKnobs: string(req.Knobs),
	}
	if err := f.store.PutMachine(ctx, row); err != nil {
		return nil, err
	}
	f.healthy[id] = true
	return row, nil
}

func (f *fakeMachines) Destroy(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("destroy:%s", id)
	return f.store.DeleteMachine(ctx, id)
}

func (f *fakeMachines) Suspend(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("suspend:%s", id)
	f.suspends = append(f.suspends, id)
	return nil
}

func (f *fakeMachines) Wake(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("wake:%s", id)
	return nil
}

func (f *fakeMachines) Redeploy(ctx context.Context, id string,
	req api.RedeployRequest) (*state.Machine, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("redeploy:%s:%s", id, req.Image)
	row, err := f.store.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	row.ReleaseID = req.Release
	row.State = "running"
	if err := f.store.PutMachine(ctx, row); err != nil {
		return nil, err
	}
	f.healthy[id] = f.healthyAfterRedeploy
	return row, nil
}

func (f *fakeMachines) Touch(ctx context.Context, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("touch:%s", id)
	f.touches = append(f.touches, id)
}

func (f *fakeMachines) Exec(ctx context.Context, id string, req api.ExecRequest) (*api.ExecResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthy[id] {
		return &api.ExecResponse{ExitCode: 0}, nil
	}
	return &api.ExecResponse{ExitCode: 1, Stderr: "not ready"}, nil
}

func (f *fakeMachines) Checkpoint(ctx context.Context, id, comment string) (*state.Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("checkpoint:%s", id)
	if f.noSnap {
		return &state.Checkpoint{ID: "ck-1", MachineID: id}, nil
	}
	return &state.Checkpoint{ID: "ck-1", MachineID: id, MemBuildID: "mem-1", RootfsBuildID: "rootfs-1"}, nil
}

func (f *fakeMachines) AppAddr(id string) (string, bool) { return "", false }

func (f *fakeMachines) ResetAgentToken(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("reset-token:%s", id)
	return nil
}

func fixture(t *testing.T, replicas int) (*Manager, *fakeMachines, state.Store, *state.Service) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	svc := &state.Service{
		ID: "svc-1", Name: "web", App: "shop", Replicas: replicas,
		// A command check, so the fake needs no HTTP listener.
		Health: `{"type":"cmd","test":["CMD-SHELL","true"],"grace":2,"interval":1,"healthy_threshold":1}`,
	}
	if err := store.PutService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	fm := newFakeMachines(store)
	return New(Options{HostID: "host-a", Store: store, Machines: fm}), fm, store, svc
}

// The first replica of a release boots; every one after it restores.
//
// This is the difference between a deploy landing on the measured sub-second
// path and every replica paying a cold boot nobody has budgeted. e2b's
// template builds end in exactly this shape -- a memory snapshot taken after
// the start command ran, so the first create never cold boots.
func TestOnlyTheFirstReplicaBoots(t *testing.T) {
	m, fm, _, _ := fixture(t, 3)

	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-build", nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	var how []string
	for _, e := range fm.events {
		if strings.HasPrefix(e, "create:") {
			how = append(how, strings.Split(e, ":")[2])
		}
	}
	want := []string{"boot", "restore", "restore"}
	if fmt.Sprint(how) != fmt.Sprint(want) {
		t.Errorf("replica creation was %v, want %v\nevents: %v", how, want, fm.events)
	}
}

// The credential is reset to the placeholder BEFORE the snapshot.
//
// A release image carrying the first replica's own token locks every later
// replica out of its own agent -- and it fails at install time, long after the
// deploy has reported the snapshot succeeded.
func TestTheTokenIsResetBeforeTheSnapshot(t *testing.T) {
	m, fm, _, _ := fixture(t, 2)
	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-build", nil); err != nil {
		t.Fatal(err)
	}

	reset, ckpt := -1, -1
	for i, e := range fm.events {
		if strings.HasPrefix(e, "reset-token:") && reset < 0 {
			reset = i
		}
		if strings.HasPrefix(e, "checkpoint:") && ckpt < 0 {
			ckpt = i
		}
	}
	if reset < 0 || ckpt < 0 {
		t.Fatalf("expected a token reset and a checkpoint, got %v", fm.events)
	}
	if reset > ckpt {
		t.Errorf("the token was reset AFTER the snapshot (%d > %d): every replica "+
			"restored from it would be locked out of its own agent\nevents: %v",
			reset, ckpt, fm.events)
	}
}

// A release with no memory image still deploys -- by booting.
//
// sprites discards memory images on upgrade, disk pressure and migration, so a
// platform that failed the deploy because a snapshot was missing would be
// worse than one that took the slow path. Restore-first, boot-second.
func TestAReleaseWithNoSnapshotStillDeploys(t *testing.T) {
	m, fm, _, _ := fixture(t, 2)
	fm.noSnap = true

	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-build", nil); err != nil {
		t.Fatalf("a snapshotless release failed to deploy: %v", err)
	}
	for _, e := range fm.events {
		if e == "create:m-2:restore" {
			t.Error("replica 2 restored from a release that has no memory image")
		}
	}
}

// The route is flipped only after the new replicas are healthy, and a deploy
// that fails leaves the old release serving.
func TestAFailedDeployDoesNotFlipTheRoute(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := fixture(t, 2)

	// Land an initial release so there is something to protect.
	first, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := store.GetService(ctx, "svc-1")
	if svc.ReleaseID != first.ID {
		t.Fatalf("first deploy did not flip: %q", svc.ReleaseID)
	}

	// Now fail the second replica of the next deploy.
	fm.creates, fm.failNth = 0, 2
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil); err == nil {
		t.Fatal("a deploy whose replica could not be created reported success")
	}

	svc, _ = store.GetService(ctx, "svc-1")
	if svc.ReleaseID != first.ID {
		t.Errorf("the route moved to a release that never became healthy: %q", svc.ReleaseID)
	}
	// And it cleaned up after itself rather than leaving half a rollout billing.
	var destroyed int
	for _, e := range fm.events {
		if strings.HasPrefix(e, "destroy:") {
			destroyed++
		}
	}
	if destroyed == 0 {
		t.Errorf("the failed deploy left its machines behind: %v", fm.events)
	}
}

// The superseded release is suspended, not destroyed -- rollback is a wake and
// a flip, and 5b deletes the service row with the last machine referencing it.
func TestTheOldReleaseIsSuspendedNotDestroyed(t *testing.T) {
	ctx := context.Background()
	m, fm, _, _ := fixture(t, 1)

	if _, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil); err != nil {
		t.Fatal(err)
	}
	fm.events = nil
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil); err != nil {
		t.Fatal(err)
	}

	if len(fm.suspends) == 0 {
		t.Errorf("the superseded replica was not suspended: %v", fm.events)
	}
	for _, e := range fm.events {
		if e == "destroy:m-1" {
			t.Error("the superseded replica was destroyed; rollback is now a rebuild, " +
				"and with it went the service row that carries the sealed environment")
		}
	}
}

// Rollback wakes the previous release's machines and flips back to it.
func TestRollbackWakesThepreviousRelease(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := fixture(t, 1)

	first, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil); err != nil {
		t.Fatal(err)
	}
	fm.events = nil

	back, err := m.Rollback(ctx, "svc-1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.ID != first.ID {
		t.Errorf("rolled back to %q, want the first release %q", back.ID, first.ID)
	}
	svc, _ := store.GetService(ctx, "svc-1")
	if svc.ReleaseID != first.ID {
		t.Errorf("the route did not move back: %q", svc.ReleaseID)
	}
	var woke bool
	for _, e := range fm.events {
		if strings.HasPrefix(e, "wake:") {
			woke = true
		}
	}
	if !woke {
		t.Errorf("rollback rebuilt instead of waking: %v", fm.events)
	}
}

// Promote must not touch the machine's identity.
//
// An agent iterating against a sandbox has been using its URL for an hour, and
// every checkpoint it took restores into that machine. A promote that mints a
// new machine has failed even if everything serves -- the URL its user has
// open stops working, and its checkpoint history points at a machine that is
// no longer the one running.
func TestPromoteKeepsTheMachinesIdentity(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := fixture(t, 1)

	sandbox := &state.Machine{
		ID: "m-sandbox", Name: "lively-hill-42", HostID: "host-a", State: "running",
		Domain: "lively-hill-42.pilotrun.app", App: "shop",
		AgentTokenHash: "hash-abc",
	}
	if err := store.PutMachine(ctx, sandbox); err != nil {
		t.Fatal(err)
	}

	svc, err := m.Promote(ctx, "m-sandbox", api.PromoteRequest{})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	after, err := store.GetMachine(ctx, "m-sandbox")
	if err != nil {
		t.Fatalf("the promoted machine is gone: %v", err)
	}
	if after.ID != sandbox.ID || after.Name != sandbox.Name || after.Domain != sandbox.Domain {
		t.Errorf("identity changed: id %q->%q name %q->%q domain %q->%q",
			sandbox.ID, after.ID, sandbox.Name, after.Name, sandbox.Domain, after.Domain)
	}
	if after.AgentTokenHash != sandbox.AgentTokenHash {
		t.Error("the agent token changed; every exec the caller had open is now unauthorized")
	}
	if after.ServiceID != svc.ID || after.ReleaseID == "" {
		t.Errorf("the machine was not bound to its new service/release: %+v", after)
	}
	// It became replica one, not a second machine.
	for _, e := range fm.events {
		if strings.HasPrefix(e, "create:") {
			t.Errorf("promote created a machine instead of adopting the sandbox: %v", fm.events)
		}
	}
}

// The promoted release is snapshotted with the placeholder credential, like
// any other release, or its scale-up replicas cannot install their own tokens.
func TestPromoteResetsTheTokenBeforeSnapshotting(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := fixture(t, 1)
	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-sandbox", Name: "sandbox", HostID: "host-a", State: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Promote(ctx, "m-sandbox", api.PromoteRequest{}); err != nil {
		t.Fatal(err)
	}
	reset, ckpt := -1, -1
	for i, e := range fm.events {
		if e == "reset-token:m-sandbox" {
			reset = i
		}
		if e == "checkpoint:m-sandbox" {
			ckpt = i
		}
	}
	if reset < 0 || ckpt < 0 || reset > ckpt {
		t.Errorf("token reset (%d) must precede the snapshot (%d): %v", reset, ckpt, fm.events)
	}
}

// replicasOfRelease returns a service's machine rows for one release.
func replicasOfRelease(t *testing.T, store state.Store, serviceID, releaseID string) []state.Machine {
	t.Helper()
	all, err := store.ListMachines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []state.Machine
	for _, mach := range all {
		if mach.ServiceID == serviceID && mach.ReleaseID == releaseID {
			out = append(out, mach)
		}
	}
	return out
}

// A deployed replica is an ordinary machine with a release attached, so it
// gets the ordinary machine defaults: suspend when idle, wake on demand, a
// floor of zero. The release is what hands its idle decision to the
// autoscaler, so nothing about the knobs has to differ between the two faces.
func TestAReplicaDefaultsToTheMachineDefaults(t *testing.T) {
	ctx := context.Background()
	m, _, store, _ := fixture(t, 1)

	rel, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	reps := replicasOfRelease(t, store, "svc-1", rel.ID)
	if len(reps) != 1 {
		t.Fatalf("deploy made %d replicas, want 1", len(reps))
	}
	if got := api.ParseKnobs(reps[0].KindKnobs); got != api.DefaultKnobs() {
		t.Errorf("replica knobs = %+v, want the machine defaults %+v", got, api.DefaultKnobs())
	}
}

// Nothing is migrated. A service whose replicas already carry a floor of one
// keeps it across the next deploy, because the rollout inherits from the
// previous release's replicas -- which are suspended and kept, never
// destroyed.
func TestAnExistingFloorSurvivesTheNextDeploy(t *testing.T) {
	ctx := context.Background()
	m, _, store, _ := fixture(t, 1)

	first, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	// The shape a service deployed before this change carries.
	old := replicasOfRelease(t, store, "svc-1", first.ID)[0]
	old.KindKnobs = `{"auto_stop":"off","min_machines_running":1}`
	if err := store.PutMachine(ctx, &old); err != nil {
		t.Fatal(err)
	}

	second, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil)
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	fresh := replicasOfRelease(t, store, "svc-1", second.ID)
	if len(fresh) != 1 {
		t.Fatalf("second deploy made %d replicas, want 1", len(fresh))
	}
	if got := api.ParseKnobs(fresh[0].KindKnobs); got.MinMachinesRunning != 1 || got.AutoStop != "off" {
		t.Errorf("a redeploy migrated an existing service's policy to the new "+
			"default: %+v", got)
	}
}

// Knobs travel on the deploy, which is the operator's opt-in for a warm
// replica, and every replica of one rollout gets the same value: resolving per
// replica would inherit from the PREVIOUS release for replica two, because the
// service still names that release while the rollout is running.
func TestKnobsOnTheDeployWinAndAreSharedByEveryReplica(t *testing.T) {
	ctx := context.Background()
	m, _, store, _ := fixture(t, 3)

	rel, err := m.Deploy(ctx, "svc-1", "rootfs-1", json.RawMessage(`{"min_machines_running":1}`))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	reps := replicasOfRelease(t, store, "svc-1", rel.ID)
	if len(reps) != 3 {
		t.Fatalf("deploy made %d replicas, want 3", len(reps))
	}
	for _, r := range reps {
		got := api.ParseKnobs(r.KindKnobs)
		if got.MinMachinesRunning != 1 {
			t.Errorf("%s has floor %d, want 1", r.ID, got.MinMachinesRunning)
		}
		// Partial merge: the fields the deploy did not mention are the
		// defaults, not the zero value. auto_stop off here would mean a
		// replica that can never be given back.
		if got.AutoStop != "suspend" || !got.AutoStart {
			t.Errorf("%s: the merge replaced instead of merging: %+v", r.ID, got)
		}
	}
}

// Promote writes nothing to the machine's knobs, and every extra replica
// inherits from the promoted machine rather than from the defaults. That is
// only true because the knobs are resolved AFTER the machine is bound to its
// release; resolving earlier would make the promoted machine invisible to the
// lookup that finds its siblings.
func TestPromoteLeavesTheKnobsAlone(t *testing.T) {
	ctx := context.Background()
	m, _, store, _ := fixture(t, 1)

	const marker = `{"auto_stop":"suspend","auto_start":true,"min_machines_running":0,"soft_limit":7}`
	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-sandbox", Name: "sandbox", HostID: "host-a", State: "running",
		App: "shop", KindKnobs: marker,
	}); err != nil {
		t.Fatal(err)
	}

	svc, err := m.Promote(ctx, "m-sandbox", api.PromoteRequest{Replicas: 2})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	after, err := store.GetMachine(ctx, "m-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if after.KindKnobs != marker {
		t.Errorf("promote rewrote the machine's knobs: %q", after.KindKnobs)
	}

	for _, r := range replicasOfRelease(t, store, svc.ID, after.ReleaseID) {
		if r.ID == "m-sandbox" {
			continue
		}
		if got := api.ParseKnobs(r.KindKnobs); got.SoftLimit != 7 {
			t.Errorf("%s inherited %+v rather than the promoted machine's policy", r.ID, got)
		}
	}
}

// A health probe must never keep a machine awake.
//
// It is exempt by construction rather than by a rule: the HTTP probe dials the
// veth's host-side IPv4 address straight from an http.Client, so it passes
// neither the router nor the IPv6 forward hook where activity is counted, and
// the command probe runs only while a rollout is gating. This pins the half
// that is testable here -- a deploy never touches a replica's row itself.
func TestTheHealthProbeDoesNotTouchTheReplica(t *testing.T) {
	ctx := context.Background()
	m, fm, _, _ := fixture(t, 2)

	if _, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(fm.touches) != 0 {
		t.Errorf("a health-gated deploy recorded activity on %v", fm.touches)
	}
}

// fakePeers records the calls a rollout makes to another host, so a test can
// tell "sent to the owner" from "done locally on the wrong host".
type fakePeers struct {
	posts     []string
	postJSONs []string
	bodies    []any
	err       error
}

func (p *fakePeers) Post(_ context.Context, hostID, path string) error {
	p.posts = append(p.posts, hostID+" "+path)
	return p.err
}

func (p *fakePeers) PostJSON(_ context.Context, hostID, path string, body any) error {
	p.postJSONs = append(p.postJSONs, hostID+" "+path)
	p.bodies = append(p.bodies, body)
	return p.err
}

// volumeFixture is one service that mounts one volume.
func volumeFixture(t *testing.T) (*Manager, *fakeMachines, state.Store, *state.Service) {
	t.Helper()
	m, fm, store, svc := fixture(t, 1)
	ctx := context.Background()
	if err := store.PutServiceVolume(ctx,
		&state.ServiceVolume{ServiceID: svc.ID, Ordinal: 1, VolumeID: "vol-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutVolume(ctx, &state.Volume{
		ID: "vol-1", Name: "data", SizeMiB: 1024, HostID: "host-a", MountPath: "/data",
	}); err != nil {
		t.Fatal(err)
	}
	return m, fm, store, svc
}

// eventsWithPrefix returns the logged events starting with prefix.
func eventsWithPrefix(fm *fakeMachines, prefix string) []string {
	var out []string
	for _, e := range fm.events {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// The first deploy of a volume-backed service boots its one replica WITH the
// volume, and takes no checkpoint: a snapshot of a volume machine carries the
// drive in its device state, and nothing may ever restore it into another
// machine.
func TestAVolumeServiceBootsItsOneReplicaWithTheVolume(t *testing.T) {
	m, fm, _, _ := volumeFixture(t)

	rel, err := m.Deploy(context.Background(), "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	creates := eventsWithPrefix(fm, "create:")
	if len(creates) != 1 || creates[0] != "create:m-1:boot:vol=vol-1" {
		t.Errorf("creates were %v, want exactly [create:m-1:boot:vol=vol-1]\nevents: %v", creates, fm.events)
	}
	if got := eventsWithPrefix(fm, "checkpoint:"); len(got) != 0 {
		t.Errorf("a volume-backed release was checkpointed: %v", got)
	}
	if !rel.Healthy || rel.MemBuildID != "" {
		t.Errorf("release is %+v, want healthy with no memory build", *rel)
	}
}

// A second deploy replaces the ONE machine in place. A second machine could
// not mount the volume beside it, and destroying the first would take the
// service row with it.
func TestARedeployReplacesTheOneMachineInPlace(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := volumeFixture(t)

	if _, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	fm.events = nil
	rel2, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil)
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	if got := eventsWithPrefix(fm, "redeploy:"); len(got) != 1 || got[0] != "redeploy:m-1:rootfs-2" {
		t.Errorf("redeploys were %v, want exactly [redeploy:m-1:rootfs-2]\nevents: %v", got, fm.events)
	}
	for _, prefix := range []string{"create:", "suspend:", "destroy:"} {
		if got := eventsWithPrefix(fm, prefix); len(got) != 0 {
			t.Errorf("a redeploy produced %v", got)
		}
	}
	replicas, err := m.replicasOf(ctx, "svc-1", rel2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replicas) != 1 || replicas[0].ID != "m-1" {
		t.Errorf("the new release's replicas are %+v, want the same m-1", replicas)
	}
	svc, err := store.GetService(ctx, "svc-1")
	if err != nil {
		t.Fatal(err)
	}
	if svc.ReleaseID != rel2.ID {
		t.Errorf("the service names %s, want %s", svc.ReleaseID, rel2.ID)
	}
}

// A failed gate puts the machine back on the release it was on, on the same
// volume, and reports both errors. There is no old machine standing by: this
// IS the old machine.
func TestAFailedGateOnAVolumeServicePutsThePreviousReleaseBack(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := volumeFixture(t)

	rel1, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	fm.events = nil
	fm.healthyAfterRedeploy = false

	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil); err == nil {
		t.Fatal("a deploy whose gate never passed reported success")
	}
	got := eventsWithPrefix(fm, "redeploy:")
	if len(got) == 0 || got[len(got)-1] != "redeploy:m-1:rootfs-1" {
		t.Errorf("redeploys were %v, want the last one back onto rootfs-1", got)
	}
	svc, err := store.GetService(ctx, "svc-1")
	if err != nil {
		t.Fatal(err)
	}
	if svc.ReleaseID != rel1.ID {
		t.Errorf("the route flipped on a failed deploy: service names %s, want %s",
			svc.ReleaseID, rel1.ID)
	}
	row, err := store.GetMachine(ctx, "m-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.ReleaseID != rel1.ID {
		t.Errorf("the machine is on %s, want it back on %s", row.ReleaseID, rel1.ID)
	}
}

// A rollback redeploys the same machine too. The wake branch exists for a
// suspended previous-release replica, and a volume-backed service keeps none.
func TestRollbackOnAVolumeServiceRedeploysNotWakes(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := volumeFixture(t)

	rel1, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil)
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	fm.events = nil

	target, err := m.Rollback(ctx, "svc-1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if target.ID != rel1.ID {
		t.Errorf("rolled back to %s, want %s", target.ID, rel1.ID)
	}
	if got := eventsWithPrefix(fm, "redeploy:"); len(got) != 1 || got[0] != "redeploy:m-1:rootfs-1" {
		t.Errorf("redeploys were %v, want exactly [redeploy:m-1:rootfs-1]\nevents: %v", got, fm.events)
	}
	if got := eventsWithPrefix(fm, "wake:"); len(got) != 0 {
		t.Errorf("a rollback of a volume-backed service woke something: %v", got)
	}
	svc, err := store.GetService(ctx, "svc-1")
	if err != nil {
		t.Fatal(err)
	}
	if svc.ReleaseID != rel1.ID {
		t.Errorf("the service names %s after the rollback, want %s", svc.ReleaseID, rel1.ID)
	}
}

// After a rescue the arbiter and the owner routinely differ, so every deploy
// of a volume-backed service from then on goes through the owner. Doing it
// locally would boot a second copy of a machine another host is running.
func TestARemoteReplicaIsRedeployedThroughItsOwner(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := volumeFixture(t)
	peers := &fakePeers{}
	m.opts.Peers = peers

	if _, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	row, err := store.GetMachine(ctx, "m-1")
	if err != nil {
		t.Fatal(err)
	}
	row.HostID = "host-b"
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	fm.events = nil

	rel2, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil)
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	want := "host-b /v1/machines/m-1/redeploy"
	if len(peers.postJSONs) != 1 || peers.postJSONs[0] != want {
		t.Fatalf("peer calls were %v, want exactly [%s]", peers.postJSONs, want)
	}
	body, ok := peers.bodies[0].(api.RedeployRequest)
	if !ok || body.Image != "rootfs-2" || body.Release != rel2.ID {
		t.Errorf("the peer call carried %#v, want image rootfs-2 and release %s", peers.bodies[0], rel2.ID)
	}
	if got := eventsWithPrefix(fm, "redeploy:"); len(got) != 0 {
		t.Errorf("the arbiter redeployed a machine it does not hold: %v", got)
	}
}

// Two rollouts of one service racing is what the CAS only half closed: both
// would boot machines, and for a volume-backed service the second would be
// refused at the claim after the first had already been killed.
func TestARolloutRefusesToOverlap(t *testing.T) {
	m, _, _, _ := fixture(t, 1)
	if err := m.beginRollout("svc-1"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Deploy(context.Background(), "svc-1", "rootfs-1", nil)
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("a second concurrent deploy returned %v, want a refusal naming the rollout", err)
	}
	m.endRollout("svc-1")
	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-1", nil); err != nil {
		t.Errorf("the deploy after the rollout ended: %v", err)
	}
}

// Promote of a volume-backed sandbox records the binding and takes no
// checkpoint: the release names the image the sandbox booted from, which is
// what a redeploy or a rollback boots again.
func TestPromoteRecordsAVolumeAndSkipsTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	m, fm, store, _ := fixture(t, 1)

	if err := store.PutVolume(ctx, &state.Volume{
		ID: "vol-1", Name: "data", SizeMiB: 1024, HostID: "host-a", MountPath: "/data",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-9", Name: "db", HostID: "host-a", State: "running",
		VolumeID: "vol-1", ImageRef: "bld-1",
	}); err != nil {
		t.Fatal(err)
	}

	svc, err := m.Promote(ctx, "m-9", api.PromoteRequest{})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	sv, err := store.ServiceVolume(ctx, svc.ID)
	if err != nil {
		t.Fatalf("ServiceVolume: %v", err)
	}
	if sv.VolumeID != "vol-1" {
		t.Errorf("the promote bound %q, want vol-1", sv.VolumeID)
	}
	rel, err := store.GetRelease(ctx, svc.ReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.RootfsBuildID != "bld-1" || rel.MemBuildID != "" {
		t.Errorf("the release is %+v, want the sandbox's image and no memory build", *rel)
	}
	for _, prefix := range []string{"checkpoint:", "reset-token:"} {
		if got := eventsWithPrefix(fm, prefix); len(got) != 0 {
			t.Errorf("a volume-backed promote produced %v", got)
		}
	}
}

// A release's memory image belongs to the pool that photographed it, exactly
// as a machine's suspend image does. A replica created on the other vendor
// cannot load it -- Firecracker refuses a foreign vmstate at snapshot load --
// so it boots from the release's rootfs instead.
//
// A FALLBACK, not an error, and the reason is the one snapshotRelease already
// gives for a missing memory image: a platform that failed a deploy because a
// snapshot could not be used would be worse than one that took the slow path.
func TestAReplicaOnTheOtherVendorBootsFromTheRootfs(t *testing.T) {
	ctx := context.Background()
	m, fm, store, svc := fixture(t, 2)
	m.opts.Vendor = "AuthenticAMD"

	rel := &state.Release{
		ID: "rel-1", ServiceID: svc.ID,
		RootfsBuildID: "rootfs-build", MemBuildID: "mem-build",
	}
	if err := store.PutRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	// Photographed on the other vendor.
	if err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: rel.ID, Kind: state.KindRelease, Vendor: "GenuineIntel"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.createReplica(ctx, svc, rel, nil, ""); err != nil {
		t.Fatalf("createReplica: %v", err)
	}
	creates := eventsWithPrefix(fm, "create:")
	if len(creates) != 1 {
		t.Fatalf("events were %v", creates)
	}
	if how := strings.Split(creates[0], ":")[2]; how != "boot" {
		t.Fatalf("a foreign release's replica %sed; it must boot from the rootfs", how)
	}
}

// The same release on its OWN vendor restores, which is what makes a deploy
// fast. Without this half the test above would pass on a manager that never
// restores anything.
func TestAReplicaOnTheReleasesVendorRestores(t *testing.T) {
	ctx := context.Background()
	m, fm, store, svc := fixture(t, 2)
	m.opts.Vendor = "AuthenticAMD"

	rel := &state.Release{
		ID: "rel-1", ServiceID: svc.ID,
		RootfsBuildID: "rootfs-build", MemBuildID: "mem-build",
	}
	if err := store.PutRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: rel.ID, Kind: state.KindRelease, Vendor: "AuthenticAMD"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.createReplica(ctx, svc, rel, nil, ""); err != nil {
		t.Fatalf("createReplica: %v", err)
	}
	creates := eventsWithPrefix(fm, "create:")
	if how := strings.Split(creates[0], ":")[2]; how != "restore" {
		t.Fatalf("a same-vendor release's replica %sed; it must restore", how)
	}
}

// A volume-backed replica boots whatever the vendor: a drive cannot be added
// to a snapshot being restored, so the volume decides before the CPU does.
func TestAVolumeReplicaBootsWhateverTheVendor(t *testing.T) {
	ctx := context.Background()
	m, fm, store, svc := volumeFixture(t)
	m.opts.Vendor = "AuthenticAMD"

	rel := &state.Release{
		ID: "rel-1", ServiceID: svc.ID,
		RootfsBuildID: "rootfs-build", MemBuildID: "mem-build",
	}
	if err := store.PutRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if err := store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: rel.ID, Kind: state.KindRelease, Vendor: "AuthenticAMD"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.createReplica(ctx, svc, rel, nil, "vol-1"); err != nil {
		t.Fatalf("createReplica: %v", err)
	}
	creates := eventsWithPrefix(fm, "create:")
	if how := strings.Split(creates[0], ":")[2]; how != "boot" {
		t.Fatalf("a volume-backed replica %sed; a drive cannot be added to a restore", how)
	}
}

// A release with no recorded pool is one photographed before machine_cpu
// existed. It restores, which is what it did then: an absent row is not a
// reason to make every deploy pay a boot.
func TestAnUnrecordedReleaseStillRestores(t *testing.T) {
	ctx := context.Background()
	m, fm, store, svc := fixture(t, 2)
	m.opts.Vendor = "AuthenticAMD"

	rel := &state.Release{
		ID: "rel-1", ServiceID: svc.ID,
		RootfsBuildID: "rootfs-build", MemBuildID: "mem-build",
	}
	if err := store.PutRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}

	if _, err := m.createReplica(ctx, svc, rel, nil, ""); err != nil {
		t.Fatalf("createReplica: %v", err)
	}
	creates := eventsWithPrefix(fm, "create:")
	if how := strings.Split(creates[0], ":")[2]; how != "restore" {
		t.Fatalf("an unrecorded release's replica %sed; it must restore", how)
	}
}

// cpuErrStore makes GetMachineCPU fail the way a corrosion query fails: not
// with ErrNotFound, and not because the row is absent.
type cpuErrStore struct {
	state.Store
	err error
}

func (s *cpuErrStore) GetMachineCPU(ctx context.Context, id string) (*state.MachineCPU, error) {
	return nil, s.err
}

// A store that cannot say which pool photographed a release refuses the
// replica rather than guessing.
//
// The two guesses are both worse than a refused deploy. Restoring a foreign
// image fails later inside Firecracker as "corrupt snapshot", with nothing
// naming the vendor; booting instead silently turns the measured sub-second
// path into a cold boot on every replica for as long as the store is unwell,
// and a deploy that got slower for no stated reason is the kind of regression
// nobody finds. TestAnUnrecordedReleaseStillRestores is the other half: an
// ABSENT row is not a failure and must still restore.
func TestAReplicaRefusesWhenTheCPUPoolCannotBeRead(t *testing.T) {
	ctx := context.Background()
	m, fm, store, svc := fixture(t, 2)
	m.opts.Vendor = "AuthenticAMD"

	rel := &state.Release{
		ID: "rel-1", ServiceID: svc.ID,
		RootfsBuildID: "rootfs-build", MemBuildID: "mem-build",
	}
	if err := store.PutRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}

	unwell := errors.New("corrosion: query timed out")
	m.opts.Store = &cpuErrStore{Store: m.opts.Store, err: unwell}

	_, err := m.createReplica(ctx, svc, rel, nil, "")
	if !errors.Is(err, unwell) {
		t.Fatalf("createReplica with an unreadable cpu row returned %v, want the store error", err)
	}
	if !strings.Contains(err.Error(), "CPU pool") {
		t.Errorf("the error does not say the vendor lookup is why: %v", err)
	}
	if creates := eventsWithPrefix(fm, "create:"); len(creates) != 0 {
		t.Errorf("a refused replica was created anyway: %v", creates)
	}
}
