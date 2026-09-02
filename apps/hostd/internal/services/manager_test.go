package services

import (
	"context"
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
}

func newFakeMachines(store state.Store) *fakeMachines {
	return &fakeMachines{store: store, healthy: map[string]bool{}}
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
	f.log("create:%s:%s", id, how)

	row := &state.Machine{
		ID: id, Name: id, HostID: "host-a", State: "running",
		ServiceID: req.Service, ReleaseID: req.Release,
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

	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-build"); err != nil {
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
	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-build"); err != nil {
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

	if _, err := m.Deploy(context.Background(), "svc-1", "rootfs-build"); err != nil {
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
	first, err := m.Deploy(ctx, "svc-1", "rootfs-1")
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := store.GetService(ctx, "svc-1")
	if svc.ReleaseID != first.ID {
		t.Fatalf("first deploy did not flip: %q", svc.ReleaseID)
	}

	// Now fail the second replica of the next deploy.
	fm.creates, fm.failNth = 0, 2
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2"); err == nil {
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

	if _, err := m.Deploy(ctx, "svc-1", "rootfs-1"); err != nil {
		t.Fatal(err)
	}
	fm.events = nil
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2"); err != nil {
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

	first, err := m.Deploy(ctx, "svc-1", "rootfs-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-2"); err != nil {
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
