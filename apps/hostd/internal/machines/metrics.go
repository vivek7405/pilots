package machines

import (
	"sync"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// uffdStats is the handler report, aliased so the interface below reads
// without dragging the package name through it.
type uffdStats = uffd.StatsReport

// CollectMetrics folds every live handler's counters into the fleet-level
// series, and is called once per scrape.
//
// Pull, not push: the memory handlers are separate processes, so their
// counters have to be asked for. A handler whose machine has gone away stops
// answering and its contribution simply disappears from the next scrape,
// which is what should happen to the counters of a machine that no longer
// exists. A push would leave its last values in the registry looking live.
//
// Per-machine values are SUMMED here. Nothing below emits a machine_id label:
// org count is bounded, machine count is not, and a label set per machine
// melts the scrape exactly when a host is busiest. See #7's landmine on this,
// and the metrics package doc.
// retiredUffd accumulates the totals of handlers that have gone away.
//
// Without it these series DECREASE. A scrape sums the live handlers, so when a
// machine suspends its handler dies and its faults leave the total -- and a
// Prometheus counter that goes down is read as a process restart, which makes
// every rate() over it wrong. It also made an e2e assertion measure a delta of
// MINUS ten faults across a suspend/wake, which is how this was found.
//
// Keyed by machine so a handler counted once is not counted twice: a machine
// that is still alive at the next scrape contributes through the live path,
// and only a machine that has disappeared is folded in here.
type retiredUffd struct {
	mu     sync.Mutex
	lastBy map[string]uffdStats
	total  uffdStats
}

func (r *retiredUffd) observe(live map[string]uffdStats) uffdStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastBy == nil {
		r.lastBy = map[string]uffdStats{}
	}
	// Anything seen before and absent now has retired: fold its last reading
	// in permanently.
	for id, last := range r.lastBy {
		if _, ok := live[id]; !ok {
			r.retire(last)
			delete(r.lastBy, id)
		}
	}
	for id, cur := range live {
		// A machine keeps its id across a suspend and wake, but its handler
		// does not: the woken machine gets a NEW process whose counters start
		// at zero. Keyed by machine alone that reads as the same handler
		// counting backwards, and the fleet total drops by whatever the dead
		// one had done -- which is exactly what a counter must never do.
		//
		// A reading lower than the last one for this id is that reset. Retire
		// the previous handler's work before adopting the new reading.
		if last, ok := r.lastBy[id]; ok && cur.Faults < last.Faults {
			r.retire(last)
		}
		r.lastBy[id] = cur
	}
	return r.total
}

func (r *retiredUffd) retire(s uffdStats) {
	r.total.Faults += s.Faults
	r.total.BytesCopied += s.BytesCopied
	r.total.Replayed += s.Replayed
	r.total.PrefetchHit += s.PrefetchHit
}

func (m *Manager) CollectMetrics() {
	m.mu.RLock()
	handlers := make(map[string]statter, len(m.running))
	for id, mach := range m.running {
		if mach.Uffd != nil {
			handlers[id] = mach.Uffd
		}
	}
	m.mu.RUnlock()

	live := make(map[string]uffdStats, len(handlers))
	for id, h := range handlers {
		if r, ok := h.Stats(); ok {
			live[id] = r
		}
	}
	retired := m.retired.observe(live)

	faults, bytes, replayed, hit, startupPages, startupBytes := foldStats(live)
	faults += retired.Faults
	bytes += retired.BytesCopied
	replayed += retired.Replayed
	hit += retired.PrefetchHit

	// Set rather than Add: each report is a whole total, so accumulating
	// deltas here would double-count every scrape.
	metrics.UffdFaults.Set(faults)
	metrics.UffdFaultBytes.Set(bytes)
	metrics.UffdPrefetchReplayed.Set(replayed)
	metrics.UffdPrefetchHit.Set(hit)
	metrics.UffdStartupPages.Set(startupPages)
	metrics.UffdStartupBytes.Set(startupBytes)
}

// foldStats sums one scrape's worth of handler reports.
//
// Split out from CollectMetrics so the summing is testable without a live
// machine: the registry is package-level, so asserting through it would make
// every test in this package share one set of counters.
func foldStats(live map[string]uffdStats) (faults, bytes, replayed, hit, startupPages, startupBytes int64) {
	for _, r := range live {
		faults += r.Faults
		bytes += r.BytesCopied
		replayed += r.Replayed
		hit += r.PrefetchHit
		startupPages += r.StartupPages
		// Each handler's own page size, so a 2MiB machine and a 4KiB machine
		// on the same host are both counted correctly.
		startupBytes += r.StartupPages * int64(r.PageSize)
	}
	return faults, bytes, replayed, hit, startupPages, startupBytes
}

// statter is the part of uffd.Process this file needs, named so the collector
// can be tested without starting a handler process.
type statter interface {
	Stats() (uffdStats, bool)
}
