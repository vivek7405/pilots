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
	handlers := []statter{
		&fakeStatter{ok: true, report: uffd.StatsReport{
			Faults: 100, BytesCopied: 409600, Replayed: 10, PrefetchHit: 8,
			StartupPages: 40, PageSize: 4096,
		}},
		&fakeStatter{ok: true, report: uffd.StatsReport{
			Faults: 250, BytesCopied: 1024000, Replayed: 25, PrefetchHit: 20,
			StartupPages: 60, PageSize: 4096,
		}},
	}

	faults, bytes, replayed, hit, pages, startBytes := foldHandlers(handlers)

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
	gone := &fakeStatter{ok: false, report: uffd.StatsReport{Faults: 999}}
	live := &fakeStatter{ok: true, report: uffd.StatsReport{Faults: 7, PageSize: 4096}}

	faults, _, _, _, _, _ := foldHandlers([]statter{gone, live})
	if faults != 7 {
		t.Errorf("faults = %d, want 7 -- a dead handler must contribute nothing", faults)
	}
	if gone.asked != 1 {
		t.Errorf("dead handler asked %d times, want 1", gone.asked)
	}
}

// Startup bytes are derived from the handler's own page size, so a 2MiB
// machine and a 4KiB machine on the same host are both counted correctly.
func TestCollectDerivesStartupBytesFromEachPageSize(t *testing.T) {
	_, _, _, _, _, startBytes := foldHandlers([]statter{
		&fakeStatter{ok: true, report: uffd.StatsReport{StartupPages: 10, PageSize: 4096}},
		&fakeStatter{ok: true, report: uffd.StatsReport{StartupPages: 10, PageSize: 2 << 20}},
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
