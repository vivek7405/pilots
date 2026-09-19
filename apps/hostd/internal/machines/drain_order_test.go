package machines

import (
	"sync"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// Machines are handed off in the order they were sorted into.
//
// drainOrder puts the cheapest moves first, and this file's header explains at
// length why: a suspended machine's move is a row write nobody sees, while a
// volume-backed one costs an unmount, a mount and a boot. The sort ran. Then
// every machine's goroutine was created at once and they raced for the
// concurrency semaphore, which was acquired INSIDE the goroutine -- so which
// moved first was the Go scheduler's choice. The ordering was computed,
// logged, and discarded, and a drain could begin with the most
// customer-visible machine on the host.
//
// drainConcurrency is pinned to 1 here because the order is not observable
// while four move at once. Moving the acquire back inside the goroutine reds
// this: the sequence comes back shuffled.
func TestADrainHandsOffInTheOrderItSorted(t *testing.T) {
	prev := drainConcurrency
	drainConcurrency = 1
	t.Cleanup(func() { drainConcurrency = prev })

	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Written in the WRONG order on purpose, so passing cannot be an accident
	// of insertion order.
	rows := []state.Machine{
		{ID: "m_vol_1", HostID: "host-a", State: StateRunning, VolumeID: "v1"},
		{ID: "m_run_1", HostID: "host-a", State: StateRunning},
		{ID: "m_off_1", HostID: "host-a", State: StateSuspended},
		{ID: "m_vol_2", HostID: "host-a", State: StateRunning, VolumeID: "v2"},
		{ID: "m_off_2", HostID: "host-a", State: StateStopped},
		{ID: "m_run_2", HostID: "host-a", State: StateRunning},
	}
	for i := range rows {
		if err := st.PutMachine(t.Context(), &rows[i]); err != nil {
			t.Fatal(err)
		}
	}

	m := &Manager{opts: Options{HostID: "host-a", Store: st}, flight: newInFlight()}

	var mu sync.Mutex
	var order []int
	pick := func(row state.Machine) (string, bool) {
		mu.Lock()
		order = append(order, drainOrder(row))
		mu.Unlock()
		// No target, so handOff gives up immediately and the drain is only the
		// ordering. Nothing is actually moved.
		return "", false
	}

	if _, err := m.Drain(t.Context(), pick); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != len(rows) {
		t.Fatalf("%d machines were offered, want %d: %v", len(order), len(rows), order)
	}
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Errorf("machine %d was handed off at cost %d after one at cost %d; "+
				"the drain is running in the scheduler's order, not drainOrder's: %v",
				i, order[i], order[i-1], order)
			break
		}
	}
	// And the cheap ones really were first, rather than everything happening
	// to share one cost.
	if order[0] != 0 || order[len(order)-1] != 2 {
		t.Errorf("sequence = %v, want it to begin with a row write and end with a "+
			"volume move", order)
	}
}

// The drain still moves EVERY machine, whatever the order. Serialising the
// slot handout must not drop one.
func TestADrainStillOffersEveryMachine(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const n = 9
	for i := range n {
		if err := st.PutMachine(t.Context(), &state.Machine{
			ID: "m_" + string(rune('a'+i)), HostID: "host-a", State: StateSuspended,
		}); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{opts: Options{HostID: "host-a", Store: st}, flight: newInFlight()}

	var mu sync.Mutex
	seen := map[string]bool{}
	pick := func(row state.Machine) (string, bool) {
		mu.Lock()
		seen[row.ID] = true
		mu.Unlock()
		return "", false
	}

	res, err := m.Drain(t.Context(), pick)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(seen) != n {
		t.Errorf("offered %d machines, want %d", len(seen), n)
	}
	if len(res.Left) != n {
		t.Errorf("%d machines reported left, want %d: every one had nowhere to go",
			len(res.Left), n)
	}
}
