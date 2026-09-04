package machines

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

type fakeStatter struct {
	report uffd.StatsReport
	ok     bool
	asked  int
	pid    int
}

func (f *fakeStatter) Stats() (uffd.StatsReport, bool) {
	f.asked++
	return f.report, f.ok
}

func (f *fakeStatter) Pid() int { return f.pid }

var _ statter = (*fakeStatter)(nil)

// alive builds the "still has a handler" set for observe: every listed
// machine on the same pid, which is a scrape with no handler turnover.
func alive(pid int, ids ...string) map[string]int {
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		out[id] = pid
	}
	return out
}

// The whole point of the collector: N machines become ONE series each. A
// label per machine would melt the scrape on a busy host, so the sum is the
// contract, not an implementation detail.
func TestCollectSumsHandlersIntoOneSeries(t *testing.T) {
	live := map[string]uffdStats{
		"m-1": {Faults: 100, BytesCopied: 409600, Replayed: 10, PrefetchHit: 8,
			StartupPages: 40, PageSize: 4096},
		"m-2": {Faults: 250, BytesCopied: 1024000, Replayed: 25, PrefetchHit: 20,
			StartupPages: 60, PageSize: 4096},
	}

	faults, bytes, replayed, hit, pages, startBytes := foldStats(live)

	if faults != 350 {
		t.Errorf("faults = %d, want 350", faults)
	}
	if bytes != 1433600 {
		t.Errorf("bytes = %d, want 1433600", bytes)
	}
	if replayed != 35 {
		t.Errorf("replayed = %d, want 35", replayed)
	}
	if hit != 28 {
		t.Errorf("prefetch hits = %d, want 28", hit)
	}
	if pages != 100 {
		t.Errorf("startup pages = %d, want 100", pages)
	}
	if startBytes != 100*4096 {
		t.Errorf("startup bytes = %d, want %d", startBytes, 100*4096)
	}
}

// A machine that went away between the listing and the question contributes
// nothing, and must not zero the fleet's totals or fail the scrape.
func TestCollectSkipsAHandlerThatCannotAnswer(t *testing.T) {
	// A handler that cannot answer never reaches the fold at all.
	faults, _, _, _, _, _ := foldStats(map[string]uffdStats{
		"m-live": {Faults: 7, PageSize: 4096},
	})
	if faults != 7 {
		t.Errorf("faults = %d, want 7 -- a dead handler must contribute nothing", faults)
	}
}

// A Prometheus counter that goes DOWN is read as a process restart, and every
// rate() over it is then wrong. The scrape sums live handlers, so a machine
// suspending takes its faults out of the total unless they are retained.
// This is not hypothetical: it made an e2e assertion measure a delta of MINUS
// ten faults across a suspend and wake.
func TestRetiredHandlersKeepTheCountersMonotonic(t *testing.T) {
	var r retiredUffd

	// Two machines busy.
	total := r.observe(alive(1, "m-1", "m-2"), map[string]uffdStats{
		"m-1": {Faults: 100, BytesCopied: 4096, Replayed: 5, PrefetchHit: 4},
		"m-2": {Faults: 50, BytesCopied: 2048, Replayed: 3, PrefetchHit: 2},
	})
	if total.Faults != 0 {
		t.Errorf("nothing has retired yet, but the accumulator holds %d faults",
			total.Faults)
	}

	// m-2 suspends. Its handler is gone; its faults must not vanish.
	total = r.observe(alive(1, "m-1"), map[string]uffdStats{
		"m-1": {Faults: 120, BytesCopied: 8192, Replayed: 6, PrefetchHit: 5},
	})
	if total.Faults != 50 {
		t.Errorf("retired faults = %d, want 50 from the machine that went away",
			total.Faults)
	}
	if got := total.Faults + 120; got < 150 {
		t.Errorf("fleet total went backwards: %d, was 150 before the suspend", got)
	}

	// m-1 keeps running: it must NOT be double counted.
	total = r.observe(alive(1, "m-1"), map[string]uffdStats{
		"m-1": {Faults: 130, BytesCopied: 9000, Replayed: 7, PrefetchHit: 6},
	})
	if total.Faults != 50 {
		t.Errorf("retired faults = %d after a live machine was rescraped, want 50",
			total.Faults)
	}
}

// Startup bytes are derived from the handler's own page size, so a 2MiB
// machine and a 4KiB machine on the same host are both counted correctly.
func TestCollectDerivesStartupBytesFromEachPageSize(t *testing.T) {
	_, _, _, _, _, startBytes := foldStats(map[string]uffdStats{
		"m-small": {StartupPages: 10, PageSize: 4096},
		"m-huge":  {StartupPages: 10, PageSize: 2 << 20},
	})
	want := int64(10*4096 + 10*(2<<20))
	if startBytes != want {
		t.Errorf("startup bytes = %d, want %d", startBytes, want)
	}
}

// Set, not Add: each report is a whole total, so a second scrape of unchanged
// handlers must not double the series.
func TestCollectMetricsIsIdempotentAcrossScrapes(t *testing.T) {
	// Built through New: CollectMetrics now also reads the slot pool and the
	// in-flight map, neither of which a bare struct literal has.
	m := New(Options{})
	m.CollectMetrics()
	first := metrics.UffdFaults.Load()
	m.CollectMetrics()
	if second := metrics.UffdFaults.Load(); second != first {
		t.Errorf("faults went %d -> %d across two scrapes of the same state",
			first, second)
	}
}

// A machine keeps its id across a suspend and wake; its HANDLER does not. The
// woken machine gets a new process whose counters start at zero, and keyed by
// machine alone that reads as one handler counting backwards -- the fleet
// total drops by whatever the dead handler had done. This is what made the
// e2e assertion measure minus seventeen faults across a wake.
func TestRetiredHandlersSurviveACounterResetOnTheSameMachine(t *testing.T) {
	var r retiredUffd

	r.observe(alive(1, "m-1"), map[string]uffdStats{"m-1": {Faults: 100, BytesCopied: 4096}})

	// Same machine, new handler after a wake: the counter restarts.
	total := r.observe(alive(2, "m-1"), map[string]uffdStats{"m-1": {Faults: 3, BytesCopied: 128}})
	if total.Faults != 100 {
		t.Fatalf("retired faults = %d, want the dead handler's 100", total.Faults)
	}
	if fleet := total.Faults + 3; fleet < 100 {
		t.Errorf("fleet total went backwards to %d after a wake", fleet)
	}

	// And it keeps climbing from there without re-retiring.
	total = r.observe(alive(2, "m-1"), map[string]uffdStats{"m-1": {Faults: 9, BytesCopied: 512}})
	if total.Faults != 100 {
		t.Errorf("retired faults = %d after the new handler advanced, want 100",
			total.Faults)
	}
}

// The new handler is told apart by its pid, not by its count going down. A
// woken machine that has already out-faulted its previous handler by the
// first scrape looks, by count alone, like the same handler still climbing --
// and the old handler's work silently leaves the total.
func TestRetiredHandlersSurviveAResetThatOvershootsTheOldCount(t *testing.T) {
	var r retiredUffd

	r.observe(alive(1, "m-1"), map[string]uffdStats{"m-1": {Faults: 100}})

	// New pid, and it has already served more than the old one ever did.
	total := r.observe(alive(2, "m-1"), map[string]uffdStats{"m-1": {Faults: 150}})
	if total.Faults != 100 {
		t.Errorf("retired faults = %d, want the dead handler's 100; a reset "+
			"that overshoots the old count was read as the old handler", total.Faults)
	}
}

// A handler that is still there but did not answer this scrape -- the control
// socket is served one request at a time and a prefault holds it for seconds
// -- must not be retired. Retiring it folds its total in permanently, and its
// next answer is then counted on top of that: the series jumps by everything
// the machine has ever done.
func TestRetiredHandlersIgnoreAScrapeTheHandlerMissed(t *testing.T) {
	var r retiredUffd

	r.observe(alive(1, "m-1"), map[string]uffdStats{"m-1": {Faults: 100}})

	// Still alive, no answer.
	total := r.observe(alive(1, "m-1"), map[string]uffdStats{})
	if total.Faults != 0 {
		t.Fatalf("retired faults = %d after a missed scrape of a live handler, want 0",
			total.Faults)
	}

	// It answers again. 100 retired + 120 live would be 220 for a machine
	// that has faulted 120 times.
	total = r.observe(alive(1, "m-1"), map[string]uffdStats{"m-1": {Faults: 120}})
	if total.Faults != 0 {
		t.Errorf("retired faults = %d, want 0: the handler never went away",
			total.Faults)
	}
}

// pilots_machines{state} is set from the idle tick, over rows the tick already
// holds. Three things have to be true at once: only this host's rows count
// (single-writer -- a host publishes its own machines and nothing else),
// tombstones do not, and every state is written on every pass.
func TestCountByStatePublishesThisHostsRowsOnly(t *testing.T) {
	m := New(Options{HostID: "host-a"})
	m.countByState([]state.Machine{
		{ID: "m-1", HostID: "host-a", State: StateRunning},
		{ID: "m-2", HostID: "host-a", State: StateRunning},
		{ID: "m-3", HostID: "host-a", State: StateSuspended},
		{ID: "m-4", HostID: "host-a", State: state.StateDestroyed},
		{ID: "m-5", HostID: "host-b", State: StateRunning},
	})

	for _, tc := range []struct {
		state string
		want  int64
	}{
		{StateRunning, 2},
		{StateSuspended, 1},
		{StateCreating, 0},
		{StateStopped, 0},
		{StateError, 0},
	} {
		if got := metrics.Machines.With(tc.state).Load(); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.state, got, tc.want)
		}
	}
}

// A gauge that is simply not set keeps its last value. Without seeding every
// state at zero, the last machine leaving "running" leaves the series reading
// 1 forever -- which is worse than no metric, because it looks alive.
func TestCountByStateZeroesAStateThatHasEmptied(t *testing.T) {
	m := New(Options{HostID: "host-a"})
	m.countByState([]state.Machine{{ID: "m-1", HostID: "host-a", State: StateRunning}})
	if got := metrics.Machines.With(StateRunning).Load(); got != 1 {
		t.Fatalf("running = %d, want 1", got)
	}

	m.countByState(nil)
	if got := metrics.Machines.With(StateRunning).Load(); got != 0 {
		t.Errorf("running = %d after every machine went away, want 0", got)
	}
}

// Both gauges are pure in-memory reads, which is why the scrape may ask for
// them directly rather than waiting for a tick.
func TestCollectMetricsPublishesInflightAndFreeSlots(t *testing.T) {
	m := New(Options{HostID: "host-a", PoolSize: 8})

	m.CollectMetrics()
	if got := metrics.SlotsFree.Load(); got != 7 {
		t.Errorf("slots free = %d on a fresh pool of 8, want 7", got)
	}
	if got := metrics.RouterInflight.Load(); got != 0 {
		t.Errorf("in flight = %d with no requests, want 0", got)
	}

	m.Begin("m-1")
	m.Begin("m-2")
	m.CollectMetrics()
	if got := metrics.RouterInflight.Load(); got != 2 {
		t.Errorf("in flight = %d with two requests across two machines, want 2", got)
	}

	m.End("m-1")
	m.End("m-2")
	m.CollectMetrics()
	if got := metrics.RouterInflight.Load(); got != 0 {
		t.Errorf("in flight = %d after both finished, want 0", got)
	}
}
