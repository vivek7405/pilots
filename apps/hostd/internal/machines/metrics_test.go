package machines

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

type fakeStatter struct {
	report uffd.StatsReport
	ok     bool
	asked  int
}

func (f *fakeStatter) Stats() (uffd.StatsReport, bool) {
	f.asked++
	return f.report, f.ok
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
	total := r.observe(map[string]uffdStats{
		"m-1": {Faults: 100, BytesCopied: 4096, Replayed: 5, PrefetchHit: 4},
		"m-2": {Faults: 50, BytesCopied: 2048, Replayed: 3, PrefetchHit: 2},
	})
	if total.Faults != 0 {
		t.Errorf("nothing has retired yet, but the accumulator holds %d faults",
			total.Faults)
	}

	// m-2 suspends. Its handler is gone; its faults must not vanish.
	total = r.observe(map[string]uffdStats{
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
	total = r.observe(map[string]uffdStats{
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
	m := &Manager{opts: Options{}}
	m.CollectMetrics()
	first := metrics.UffdFaults.Load()
	m.CollectMetrics()
	if second := metrics.UffdFaults.Load(); second != first {
		t.Errorf("faults went %d -> %d across two scrapes of the same state",
			first, second)
	}
}
