package services

import (
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
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
