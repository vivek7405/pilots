package services

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

func knobs(min, soft int) api.Knobs {
	return api.Knobs{AutoStop: "suspend", AutoStart: true,
		MinMachinesRunning: min, SoftLimit: soft}
}

// minMachinesRunning is a FLOOR, not a target.
//
// The scale-down path is where this gets violated: a service with a floor of 1
// and demand for 5 must not be cut back to 1. Getting it backwards produces a
// service that scales up under load and immediately undoes it.
func TestTheFloorNeverCapsScaleUp(t *testing.T) {
	now := time.Now()
	// Four replicas, all at the soft limit, floor of 1.
	var reps []Replica
	for _, id := range []string{"m-1", "m-2", "m-3", "m-4"} {
		reps = append(reps, Replica{ID: id, Running: true, InFlight: 20})
	}
	d := Decide(knobs(1, 20), reps, now, map[string]time.Time{})
	if !d.Up {
		t.Error("every replica is at its limit and the floor is 1, but the " +
			"controller declined to scale up: the floor is being read as a target")
	}
	if d.Down != "" {
		t.Errorf("it also wanted to stop %s while saturated", d.Down)
	}
}

// And scale-down clamps AT the floor rather than below it.
func TestScaleDownStopsAtTheFloor(t *testing.T) {
	now := time.Now()
	idle := map[string]time.Time{
		"m-1": now.Add(-time.Hour),
		"m-2": now.Add(-time.Hour),
	}
	// Two idle replicas, floor of 2: nothing may be given back.
	reps := []Replica{
		{ID: "m-1", Running: true, InFlight: 0},
		{ID: "m-2", Running: true, InFlight: 0},
	}
	if d := Decide(knobs(2, 20), reps, now, idle); d.Down != "" {
		t.Errorf("stopped %s and broke the floor of 2", d.Down)
	}

	// A third idle replica above the floor may go.
	reps = append(reps, Replica{ID: "m-3", Running: true, InFlight: 0})
	idle["m-3"] = now.Add(-time.Hour)
	if d := Decide(knobs(2, 20), reps, now, idle); d.Down == "" {
		t.Error("three idle replicas above a floor of 2 and nothing was given back")
	}
}

// Scale-down is sustained, not instantaneous: one quiet tick is not idleness.
func TestOneQuietTickIsNotIdle(t *testing.T) {
	now := time.Now()
	reps := []Replica{
		{ID: "m-1", Running: true, InFlight: 0},
		{ID: "m-2", Running: true, InFlight: 0},
	}
	idle := map[string]time.Time{}

	// First observation records the moment; it must not act on it.
	if d := Decide(knobs(1, 20), reps, now, idle); d.Down != "" {
		t.Errorf("scaled down on the first quiet observation: %s", d.Down)
	}
	// Still too soon.
	if d := Decide(knobs(1, 20), reps, now.Add(ScaleInterval), idle); d.Down != "" {
		t.Errorf("scaled down after one interval: %s", d.Down)
	}
	// Long enough.
	if d := Decide(knobs(1, 20), reps, now.Add(idleBeforeScaleDown+time.Second), idle); d.Down == "" {
		t.Error("never scaled down despite sustained idleness")
	}
}

// Traffic on a replica cancels its idleness rather than leaving a stale clock.
func TestTrafficResetsTheIdleClock(t *testing.T) {
	now := time.Now()
	idle := map[string]time.Time{}
	reps := []Replica{
		{ID: "m-1", Running: true, InFlight: 0},
		{ID: "m-2", Running: true, InFlight: 0},
	}
	Decide(knobs(1, 20), reps, now, idle)

	// m-1 gets busy well before the idle threshold.
	reps[0].InFlight = 5
	Decide(knobs(1, 20), reps, now.Add(ScaleInterval), idle)
	if _, stillIdle := idle["m-1"]; stillIdle {
		t.Error("a replica that took traffic kept its idle clock, and would be " +
			"stopped for idleness it no longer has")
	}
}

// Below the floor is a repair, not a scaling decision: bring one back even
// with no traffic at all.
func TestBelowTheFloorIsRepaired(t *testing.T) {
	d := Decide(knobs(2, 20), []Replica{{ID: "m-1", Running: true, InFlight: 0}},
		time.Now(), map[string]time.Time{})
	if !d.Up {
		t.Error("one running replica under a floor of 2 was left there")
	}
}

// A service with no soft limit never scales up on concurrency -- there is no
// signal to scale on, and inventing one would scale on nothing.
func TestNoSoftLimitMeansNoConcurrencyScaling(t *testing.T) {
	reps := []Replica{{ID: "m-1", Running: true, InFlight: 9999}}
	if d := Decide(knobs(1, 0), reps, time.Now(), map[string]time.Time{}); d.Up {
		t.Error("scaled up on concurrency for a service that declared no limit")
	}
}

// fakeLoad is the two per-replica numbers the controller reads, with no
// router and no namespace behind them.
type fakeLoad struct{ inflight, held map[string]int }

func (f fakeLoad) InFlight(id string) int { return f.inflight[id] }
func (f fakeLoad) Held(id string) int     { return f.held[id] }

// The whole point of the change: at a floor of zero the LAST replica may be
// given back. Everything else about the service is unchanged -- it still has
// one replica, that replica still has its URL, and a request wakes it.
func TestAFloorOfZeroSuspendsTheLastIdleReplica(t *testing.T) {
	now := time.Now()
	reps := []Replica{{ID: "m-1", Running: true, LastActivity: now.Add(-time.Hour)}}
	idle := map[string]time.Time{"m-1": now.Add(-time.Hour)}

	if d := Decide(knobs(0, 20), reps, now, idle); d.Down != "m-1" {
		t.Errorf("the last idle replica of a floor-zero service was kept: %+v", d)
	}
	// The same service with a floor of one keeps it, which is the opt-in an
	// operator gets by deploying with min_machines_running set.
	if d := Decide(knobs(1, 20), reps, now, idle); d.Down != "" {
		t.Errorf("stopped %s and broke a floor of 1", d.Down)
	}
}

// auto_stop: off means never give this replica back, whatever the floor says.
// It is what the knob means for a sandbox and it was ignored here entirely.
func TestAutoStopOffNeverScalesDown(t *testing.T) {
	now := time.Now()
	k := api.Knobs{AutoStop: "off", AutoStart: true, SoftLimit: 20}
	reps := []Replica{{ID: "m-1", Running: true, LastActivity: now.Add(-time.Hour)}}
	idle := map[string]time.Time{"m-1": now.Add(-time.Hour)}

	if d := Decide(k, reps, now, idle); d.Down != "" {
		t.Errorf("auto_stop off and %s was stopped anyway", d.Down)
	}
}

// An open session keeps a replica up and never asks for another one. Twenty
// pooled connections to a database are not twenty requests: counting them as
// demand would restore a second database from the same snapshot.
func TestAHeldSessionKeepsAReplicaAndNeverScalesUp(t *testing.T) {
	now := time.Now()
	reps := []Replica{{ID: "m-1", Running: true, Held: 1, LastActivity: now.Add(-time.Hour)}}
	idle := map[string]time.Time{"m-1": now.Add(-time.Hour)}

	if d := Decide(knobs(0, 20), reps, now, idle); d.Down != "" {
		t.Errorf("suspended %s with a session open on it", d.Down)
	}

	reps[0].Held = 50
	if d := Decide(knobs(0, 20), reps, now, idle); d.Up {
		t.Error("fifty pooled connections were read as demand for a second replica")
	}
}

// last_activity is the one idle signal every host can read, and it is what the
// guest-to-guest counter writes. Ignoring it makes that counter inert.
func TestRecentActivityOnTheRowDefersScaleDown(t *testing.T) {
	now := time.Now()
	idle := map[string]time.Time{"m-1": now.Add(-time.Hour)}
	reps := []Replica{{ID: "m-1", Running: true, LastActivity: now.Add(-5 * time.Second)}}

	if d := Decide(knobs(0, 20), reps, now, idle); d.Down != "" {
		t.Errorf("suspended %s five seconds after a peer last touched its row", d.Down)
	}

	reps[0].LastActivity = now.Add(-time.Hour)
	if d := Decide(knobs(0, 20), reps, now, idle); d.Down != "m-1" {
		t.Error("a replica idle by both clocks was never given back")
	}
}

// Two hosts, one replica each, a floor of one: they must not both conclude
// they are the one to give theirs back. Prevented by ranking rather than a
// lock -- every host reads the same rows, so every host ranks the same replica
// idlest, and only its owner acts.
func TestOnlyTheOwnerGivesBackTheIdlestReplica(t *testing.T) {
	now := time.Now()
	idle := map[string]time.Time{"m-1": now.Add(-time.Hour)}
	reps := []Replica{
		{ID: "m-1", Running: true, LastActivity: now.Add(-time.Hour)},
		{ID: "m-2", Running: true, LastActivity: now.Add(-2 * time.Hour), Remote: true},
	}

	if d := Decide(knobs(1, 20), reps, now, idle); d.Down != "" {
		t.Errorf("gave back %s while a remote replica was idler; its owner is "+
			"about to give that one back and the floor would break", d.Down)
	}

	// The remote replica is busy, so the local one is now idlest fleet-wide.
	reps[1].LastActivity = now
	if d := Decide(knobs(1, 20), reps, now, idle); d.Down != "m-1" {
		t.Errorf("the idlest replica is local and was not given back: %+v", d)
	}
}

// autoscaleFixture is a two-host fleet with one service on one release, and it
// reports which host the arbiter is so a test can be the OTHER one.
func autoscaleFixture(t *testing.T) (store state.Store, arbiter, other string) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	svc := &state.Service{ID: "svc-1", Name: "web", App: "shop", Replicas: 1, ReleaseID: "rel-1"}
	if err := store.PutService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRelease(ctx, &state.Release{ID: "rel-1", ServiceID: "svc-1", Healthy: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, id := range []string{"host-a", "host-b"} {
		if err := store.PutHost(ctx, &state.Host{ID: id, LastSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	arbiter, ok := state.OwnerFor("svc-1", hosts)
	if !ok {
		t.Fatal("no arbiter for the service")
	}
	other = "host-a"
	if arbiter == other {
		other = "host-b"
	}
	return store, arbiter, other
}

const floorZeroKnobs = `{"auto_stop":"suspend","auto_start":true,"min_machines_running":0,"soft_limit":20}`

// Scale-down belongs to the host that HOLDS the replica, and scale-up to the
// arbiter.
//
// Requests are counted where they are served, so the arbiter's in-flight count
// for a replica another host holds is zero however busy it is. At a floor of
// one that was hidden. At a floor of zero an arbiter acting alone would
// suspend a busy remote replica on every pass.
func TestScaleDownRunsOnTheOwnerAndScaleUpOnTheArbiter(t *testing.T) {
	ctx := context.Background()
	store, arbiter, other := autoscaleFixture(t)
	fm := newFakeMachines(store)
	m := New(Options{HostID: other, Store: store, Machines: fm})

	row := &state.Machine{
		ID: "m-1", Name: "m-1", HostID: other, State: "running",
		ServiceID: "svc-1", ReleaseID: "rel-1", KindKnobs: floorZeroKnobs,
		LastActivity: time.Now().Add(-time.Hour).Unix(),
	}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}

	idle := map[string]time.Time{"m-1": time.Now().Add(-time.Hour)}
	for range 2 {
		if err := m.scaleOnce(ctx, fakeLoad{}, idle); err != nil {
			t.Fatalf("scaleOnce: %v", err)
		}
	}
	if len(fm.suspends) == 0 {
		t.Fatalf("the owner never gave its idle replica back: %v", fm.events)
	}

	// The same replica held by the arbiter instead: this host is neither the
	// arbiter nor the owner and must do nothing at all.
	row.HostID = arbiter
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	fm.suspends, fm.events = nil, nil
	idle["m-1"] = time.Now().Add(-time.Hour)
	if err := m.scaleOnce(ctx, fakeLoad{}, idle); err != nil {
		t.Fatalf("scaleOnce: %v", err)
	}
	if len(fm.events) != 0 {
		t.Errorf("a host that neither arbitrates nor owns the replica acted: %v", fm.events)
	}

	// The arbiter repairs a floor: one suspended replica under a floor of one
	// is woken, and waking is what scale-up does before it creates anything.
	arb := New(Options{HostID: arbiter, Store: store, Machines: fm})
	row.State = "suspended"
	row.KindKnobs = `{"auto_stop":"suspend","auto_start":true,"min_machines_running":1,"soft_limit":20}`
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	fm.events = nil
	if err := arb.scaleOnce(ctx, fakeLoad{}, map[string]time.Time{}); err != nil {
		t.Fatalf("scaleOnce: %v", err)
	}
	if !slices.Contains(fm.events, "wake:m-1") {
		t.Errorf("the arbiter did not repair the floor: %v", fm.events)
	}
}

// A busy replica keeps its own row's last_activity fresh, which is what lets
// every other host rank it as busy. Without it the fleet-wide ranking reads a
// stale row and the idlest replica is whichever one nobody reached over HTTP.
func TestABusyReplicaTouchesItsRow(t *testing.T) {
	ctx := context.Background()
	store, _, other := autoscaleFixture(t)
	fm := newFakeMachines(store)
	m := New(Options{HostID: other, Store: store, Machines: fm})

	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-1", Name: "m-1", HostID: other, State: "running",
		ServiceID: "svc-1", ReleaseID: "rel-1", KindKnobs: floorZeroKnobs,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		load fakeLoad
		want bool
	}{
		{"a request in flight", fakeLoad{inflight: map[string]int{"m-1": 1}}, true},
		{"a session held open", fakeLoad{held: map[string]int{"m-1": 1}}, true},
		{"nothing at all", fakeLoad{}, false},
	} {
		fm.touches = nil
		if err := m.scaleOnce(ctx, tc.load, map[string]time.Time{}); err != nil {
			t.Fatalf("%s: scaleOnce: %v", tc.name, err)
		}
		if got := slices.Contains(fm.touches, "m-1"); got != tc.want {
			t.Errorf("%s: touched = %v, want %v", tc.name, got, tc.want)
		}
	}
}
