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
// Keyed by machine, and the reading remembers which handler PROCESS it came
// from. A machine keeps its id across a suspend and wake but its handler does
// not: the woken machine gets a new process whose counters start at zero, and
// keyed by machine alone that reads as one handler counting backwards. The
// pid is what tells a new handler from the old one; a "counter went down"
// heuristic misses the reset whenever the new handler has already served more
// faults than the old one had when it was last scraped.
type retiredUffd struct {
	mu     sync.Mutex
	lastBy map[string]handlerReading
	total  uffdStats
}

// handlerReading is one scrape's answer from one handler process.
type handlerReading struct {
	pid   int
	stats uffdStats
}

// observe folds in every handler that has gone since the last scrape and
// returns the running total of retired work.
//
// alive is every machine that still HAS a handler this scrape, keyed to its
// pid, whether or not that handler answered; live is the readings from the
// ones that did. The two are separate on purpose: a handler that merely
// failed to answer -- the control socket is served one request at a time, and
// a prefault holds it for seconds -- must not be retired, because its next
// answer would then be counted on top of its retired total. Only a handler
// that is gone, or replaced by a new pid, retires.
func (r *retiredUffd) observe(alive map[string]int, live map[string]uffdStats) uffdStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastBy == nil {
		r.lastBy = map[string]handlerReading{}
	}
	for id, last := range r.lastBy {
		if pid, ok := alive[id]; !ok || pid != last.pid {
			r.retire(last.stats)
			delete(r.lastBy, id)
		}
	}
	for id, cur := range live {
		if last, ok := r.lastBy[id]; ok && cur.Faults < last.stats.Faults {
			// Same pid, smaller count. A handler's counters never go down,
			// so this is a pid reused between two scrapes; keep the series
			// monotonic rather than trust the coincidence.
			r.retire(last.stats)
		}
		r.lastBy[id] = handlerReading{pid: alive[id], stats: cur}
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

	alive := make(map[string]int, len(handlers))
	live := make(map[string]uffdStats, len(handlers))
	for id, h := range handlers {
		alive[id] = h.Pid()
		if r, ok := h.Stats(); ok {
			live[id] = r
		}
	}
	retired := m.retired.observe(alive, live)

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
// can be tested without starting a handler process. Pid is what tells one
// handler process from its successor on the same machine.
type statter interface {
	Stats() (uffdStats, bool)
	Pid() int
}
