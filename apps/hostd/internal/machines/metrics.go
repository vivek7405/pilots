package machines

import (
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
func (m *Manager) CollectMetrics() {
	m.mu.RLock()
	handlers := make([]statter, 0, len(m.running))
	for _, mach := range m.running {
		if mach.Uffd != nil {
			handlers = append(handlers, mach.Uffd)
		}
	}
	m.mu.RUnlock()

	faults, bytes, replayed, hit, startupPages, startupBytes := foldHandlers(handlers)

	// Set rather than Add: each report is a whole total, so accumulating
	// deltas here would double-count every scrape.
	metrics.UffdFaults.Set(faults)
	metrics.UffdFaultBytes.Set(bytes)
	metrics.UffdPrefetchReplayed.Set(replayed)
	metrics.UffdPrefetchHit.Set(hit)
	metrics.UffdStartupPages.Set(startupPages)
	metrics.UffdStartupBytes.Set(startupBytes)
}

// foldHandlers sums one scrape's worth of handler reports.
//
// Split out from CollectMetrics so the summing is testable without a live
// machine: the registry is package-level, so asserting through it would make
// every test in this package share one set of counters.
func foldHandlers(handlers []statter) (faults, bytes, replayed, hit, startupPages, startupBytes int64) {
	for _, h := range handlers {
		r, ok := h.Stats()
		if !ok {
			continue // machine gone, or adopted without a control socket
		}
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
