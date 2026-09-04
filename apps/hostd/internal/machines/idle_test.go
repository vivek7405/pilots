package machines

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

func idleRow(knobs api.Knobs, idleFor time.Duration) state.Machine {
	raw, _ := marshalKnobs(knobs)
	return state.Machine{
		ID: "m_1", State: StateRunning, KindKnobs: raw,
		LastActivity: time.Now().Add(-idleFor).Unix(),
	}
}

func testManager() *Manager {
	return &Manager{opts: Options{HostID: "host-a"}, flight: newInFlight()}
}

// storeManager is testManager with real state behind it, for the rows whose
// owner cannot be decided without reading the service.
func storeManager(t *testing.T) (*Manager, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Manager{opts: Options{HostID: "host-a", Store: st}, flight: newInFlight()}, st
}

// Both signals must agree before a machine is suspended.
//
// The timer alone would suspend a machine that is busy but generating no HTTP
// traffic -- an agent mid-build inside a sandbox looks exactly like an idle
// web app. Concurrency alone would suspend one merely between two requests.
func TestShouldSuspendRequiresTimerAndConcurrency(t *testing.T) {
	m := testManager()
	knobs := api.Knobs{AutoStop: "suspend", AutoStart: true}

	t.Run("idle long enough and nothing in flight", func(t *testing.T) {
		if !m.shouldSuspend(t.Context(), idleRow(knobs, 2*DefaultIdleTimeout)) {
			t.Error("should have suspended")
		}
	})

	t.Run("idle long enough but a request is in flight", func(t *testing.T) {
		row := idleRow(knobs, 2*DefaultIdleTimeout)
		m.flight.begin(row.ID)
		defer m.flight.end(row.ID)
		if m.shouldSuspend(t.Context(), row) {
			t.Error("suspended a machine that was still serving a request")
		}
	})

	t.Run("recently active", func(t *testing.T) {
		if m.shouldSuspend(t.Context(), idleRow(knobs, time.Second)) {
			t.Error("suspended a machine that was active a second ago")
		}
	})
}

func TestShouldSuspendHonoursKnobs(t *testing.T) {
	m := testManager()
	longIdle := 2 * DefaultIdleTimeout

	t.Run("autoStop off never suspends", func(t *testing.T) {
		row := idleRow(api.Knobs{AutoStop: "off"}, longIdle)
		if m.shouldSuspend(t.Context(), row) {
			t.Error("suspended a machine with autoStop off")
		}
	})

	// A service holding a floor of running instances is not a scale-to-zero
	// candidate, however quiet it is.
	t.Run("minMachinesRunning above zero never suspends", func(t *testing.T) {
		row := idleRow(api.Knobs{AutoStop: "suspend", MinMachinesRunning: 1}, longIdle)
		if m.shouldSuspend(t.Context(), row) {
			t.Error("suspended a machine with a running floor")
		}
	})

	t.Run("scale to zero is allowed for a service", func(t *testing.T) {
		row := idleRow(api.Knobs{AutoStop: "suspend", MinMachinesRunning: 0}, longIdle)
		if !m.shouldSuspend(t.Context(), row) {
			t.Error("a production service with minMachinesRunning=0 must scale to zero")
		}
	})
}

func TestInFlightCounting(t *testing.T) {
	f := newInFlight()

	if f.count("m_1") != 0 {
		t.Fatal("a fresh machine should have nothing in flight")
	}
	f.begin("m_1")
	f.begin("m_1")
	if got := f.count("m_1"); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	f.end("m_1")
	if got := f.count("m_1"); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	f.end("m_1")
	if got := f.count("m_1"); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}

	// Underflow must not wrap into a permanently non-zero count, or the
	// machine could never be suspended again.
	f.end("m_1")
	if got := f.count("m_1"); got != 0 {
		t.Errorf("count went negative: %d", got)
	}
}

func TestInFlightIsPerMachine(t *testing.T) {
	f := newInFlight()
	f.begin("m_1")
	if f.count("m_2") != 0 {
		t.Error("one machine's traffic leaked into another's count")
	}
}

func TestParseKnobsFallsBackToReachableDefaults(t *testing.T) {
	// A machine whose knobs are missing or corrupt must stay reachable: a
	// default of autoStart=false would strand it permanently.
	for _, raw := range []string{"", "{", "not json"} {
		k := ParseKnobs(raw)
		if !k.AutoStart {
			t.Errorf("ParseKnobs(%q) produced autoStart=false, stranding the machine", raw)
		}
	}

	k := ParseKnobs(`{"auto_stop":"off","auto_start":false,"min_machines_running":2,"soft_limit":5}`)
	if k.AutoStop != "off" || k.AutoStart || k.MinMachinesRunning != 2 || k.SoftLimit != 5 {
		t.Errorf("round trip lost values: %+v", k)
	}
}

// The stored blob is the serialised API object, so no translation layer is
// needed between the wire format and what is persisted.
func TestKnobsRoundTripThroughStorage(t *testing.T) {
	want := api.Knobs{AutoStop: "suspend", AutoStart: true, MinMachinesRunning: 1, SoftLimit: 42}
	raw, err := marshalKnobs(want)
	if err != nil {
		t.Fatalf("marshalKnobs: %v", err)
	}
	if got := ParseKnobs(raw); got != want {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}
}

func TestGeneratedNamesAreDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		n := generateName()
		if seen[n] {
			t.Fatalf("duplicate generated name %q after %d draws", n, i)
		}
		seen[n] = true
	}
}

// A machine with a release has exactly one controller, and it is not this one.
//
// The autoscaler reads the same knobs plus the service's floor and gives the
// replica back to the host that holds it. Both loops acting on one machine is
// the race the split exists to prevent: this monitor suspends on its own
// sixty-second clock while the autoscaler is still counting the replica as
// running capacity. The discriminator is the release rather than a knob,
// because a promoted sandbox keeps the knobs it was created with.
func TestTheIdleMonitorLeavesServiceReplicasToTheAutoscaler(t *testing.T) {
	m, st := storeManager(t)
	knobs := api.Knobs{AutoStop: "suspend", AutoStart: true}
	putService(t, st, "svc-1", "rel-1")

	replica := idleRow(knobs, 2*DefaultIdleTimeout)
	replica.ServiceID = "svc-1"
	replica.ReleaseID = "rel-1"
	if m.shouldSuspend(t.Context(), replica) {
		t.Error("the idle monitor suspended a service replica behind the autoscaler's back")
	}

	// The same row without a release is an ordinary sandbox and still suspends.
	if !m.shouldSuspend(t.Context(), idleRow(knobs, 2*DefaultIdleTimeout)) {
		t.Error("a sandbox with no release stopped suspending")
	}
}

// Every running machine needs exactly one controller.
//
// Stepping aside for any release id at all left a superseded replica with
// none: the autoscaler only enumerates the CURRENT release's replicas, so a
// replica of the release the service rolled off is in neither set. Deploy and
// Rollback suspend those themselves, but best-effort -- one failed Suspend
// and a Firecracker runs and bills forever with nothing that will ever
// reconsider it. That backstop is this monitor.
func TestASupersededReplicaIsStillTheIdleMonitorsToSuspend(t *testing.T) {
	m, st := storeManager(t)
	knobs := api.Knobs{AutoStop: "suspend", AutoStart: true}
	putService(t, st, "svc-1", "rel-2")

	superseded := idleRow(knobs, 2*DefaultIdleTimeout)
	superseded.ServiceID = "svc-1"
	superseded.ReleaseID = "rel-1"
	if !m.shouldSuspend(t.Context(), superseded) {
		t.Error("a replica of a superseded release was left running with no controller at all")
	}

	current := idleRow(knobs, 2*DefaultIdleTimeout)
	current.ServiceID = "svc-1"
	current.ReleaseID = "rel-2"
	if m.shouldSuspend(t.Context(), current) {
		t.Error("the current release's replica is the autoscaler's, not this monitor's")
	}
}

// A floor is a property of a release's replica SET, and a superseded release
// has no set left to keep warm -- the traffic went to the new release the
// moment the service row flipped. Honouring it here would leave the leak open
// for exactly the warm services that set a floor in the first place.
func TestASupersededReplicaSuspendsDespiteAFloor(t *testing.T) {
	m, st := storeManager(t)
	putService(t, st, "svc-1", "rel-2")

	row := idleRow(api.Knobs{AutoStop: "suspend", AutoStart: true, MinMachinesRunning: 2},
		2*DefaultIdleTimeout)
	row.ServiceID = "svc-1"
	row.ReleaseID = "rel-1"
	if !m.shouldSuspend(t.Context(), row) {
		t.Error("a superseded replica kept running to satisfy a floor its release no longer has")
	}

	// autoStop off is an explicit operator instruction and still wins.
	row.KindKnobs, _ = marshalKnobs(api.Knobs{AutoStop: "off"})
	if m.shouldSuspend(t.Context(), row) {
		t.Error("suspended a machine with autoStop off")
	}
}

// A replica whose service row is gone is nobody's either: nothing enumerates
// it, so it falls to the idle monitor rather than to no one.
func TestAReplicaOfADeletedServiceStillSuspends(t *testing.T) {
	m, _ := storeManager(t)

	row := idleRow(api.Knobs{AutoStop: "suspend", AutoStart: true}, 2*DefaultIdleTimeout)
	row.ServiceID = "svc-gone"
	row.ReleaseID = "rel-1"
	if !m.shouldSuspend(t.Context(), row) {
		t.Error("a replica of a service that no longer exists was left with no controller")
	}
}

// When the read that decides ownership is the thing that failed, take the
// reversible side. A machine left running until the next tick reads the row
// is recoverable; a second controller racing the autoscaler on a live replica
// is the exact mid-flight suspend the split exists to prevent.
func TestAnUnreadableServiceLeavesTheReplicaAlone(t *testing.T) {
	m, st := storeManager(t)
	m.opts.Store = brokenServiceStore{Store: st}

	row := idleRow(api.Knobs{AutoStop: "suspend", AutoStart: true}, 2*DefaultIdleTimeout)
	row.ServiceID = "svc-1"
	row.ReleaseID = "rel-1"
	if m.shouldSuspend(t.Context(), row) {
		t.Error("suspended a replica whose owner could not be determined")
	}

	// A sandbox never reads the service at all, so it is unaffected.
	if !m.shouldSuspend(t.Context(), idleRow(api.Knobs{AutoStop: "suspend"}, 2*DefaultIdleTimeout)) {
		t.Error("a store that cannot answer for services stopped sandboxes suspending")
	}
}

// brokenServiceStore answers everything normally except the one read the
// ownership decision turns on.
type brokenServiceStore struct {
	state.Store
}

func (brokenServiceStore) GetService(context.Context, string) (*state.Service, error) {
	return nil, errors.New("brokenServiceStore: injected failure")
}

func putService(t *testing.T, st state.Store, id, releaseID string) {
	t.Helper()
	if err := st.PutService(t.Context(), &state.Service{
		ID: id, Name: id, ReleaseID: releaseID, Replicas: 1,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
}
