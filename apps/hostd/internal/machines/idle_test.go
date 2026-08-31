package machines

import (
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

// Both signals must agree before a machine is suspended.
//
// The timer alone would suspend a machine that is busy but generating no HTTP
// traffic -- an agent mid-build inside a sandbox looks exactly like an idle
// web app. Concurrency alone would suspend one merely between two requests.
func TestShouldSuspendRequiresTimerAndConcurrency(t *testing.T) {
	m := testManager()
	knobs := api.Knobs{AutoStop: "suspend", AutoStart: true}

	t.Run("idle long enough and nothing in flight", func(t *testing.T) {
		if !m.shouldSuspend(idleRow(knobs, 2*DefaultIdleTimeout)) {
			t.Error("should have suspended")
		}
	})

	t.Run("idle long enough but a request is in flight", func(t *testing.T) {
		row := idleRow(knobs, 2*DefaultIdleTimeout)
		m.flight.begin(row.ID)
		defer m.flight.end(row.ID)
		if m.shouldSuspend(row) {
			t.Error("suspended a machine that was still serving a request")
		}
	})

	t.Run("recently active", func(t *testing.T) {
		if m.shouldSuspend(idleRow(knobs, time.Second)) {
			t.Error("suspended a machine that was active a second ago")
		}
	})
}

func TestShouldSuspendHonoursKnobs(t *testing.T) {
	m := testManager()
	longIdle := 2 * DefaultIdleTimeout

	t.Run("autoStop off never suspends", func(t *testing.T) {
		row := idleRow(api.Knobs{AutoStop: "off"}, longIdle)
		if m.shouldSuspend(row) {
			t.Error("suspended a machine with autoStop off")
		}
	})

	// A service holding a floor of running instances is not a scale-to-zero
	// candidate, however quiet it is.
	t.Run("minMachinesRunning above zero never suspends", func(t *testing.T) {
		row := idleRow(api.Knobs{AutoStop: "suspend", MinMachinesRunning: 1}, longIdle)
		if m.shouldSuspend(row) {
			t.Error("suspended a machine with a running floor")
		}
	})

	t.Run("scale to zero is allowed for a service", func(t *testing.T) {
		row := idleRow(api.Knobs{AutoStop: "suspend", MinMachinesRunning: 0}, longIdle)
		if !m.shouldSuspend(row) {
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
