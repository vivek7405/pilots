package metrics

import (
	"sort"
	"sync"
	"time"
)

// Liveness for hostd's own background loops.
//
// # The failure this exists for
//
// `Restart=always` catches a process that dies. None of these loops die. They
// wedge: a query with no timeout, a socket that accepts and never answers, a
// mutex held by something that is itself waiting. The process stays up, the
// port stays open, health answers 200, and the work silently stops. Fly lost
// five hours of billing AND certificate renewal to exactly this on 2026-09-02,
// with every process healthy throughout.
//
// So each loop says when it last completed a pass, with a budget for how long
// a healthy gap can be. Two consumers read that: a gauge per loop, which is
// what an operator alerts on, and the systemd watchdog, which is what turns a
// wedged daemon into a restarted one without anyone watching. The watchdog is
// the part that matters at three in the morning: a pet withheld is a restart,
// and a restart of hostd does not touch the machines (KillMode=process).
//
// # Why a registry rather than a field per loop
//
// A loop that is added later and forgets to register is the whole failure
// again. Registration is one line at the top of the loop, the name is the
// metric label, and AGENTS.md says a new loop registers a budget. Nothing here
// enumerates the loops, so nothing here goes stale.

// Loop is one background loop's liveness.
type Loop struct {
	name   string
	budget time.Duration

	mu   sync.Mutex
	last time.Time
}

var (
	loopsMu sync.Mutex
	loops   = map[string]*Loop{}
)

// NewLoop registers a loop and its budget: the longest gap between passes that
// is still healthy. Ordinarily three times the loop's own interval, so a
// single slow pass is not a page.
//
// Registering the same name twice returns the existing loop rather than
// replacing it, so a test that builds two managers does not silently reset the
// production one's clock.
func NewLoop(name string, budget time.Duration) *Loop {
	loopsMu.Lock()
	defer loopsMu.Unlock()
	if l, ok := loops[name]; ok {
		return l
	}
	l := &Loop{name: name, budget: budget, last: time.Now()}
	loops[name] = l
	LoopLastTick.With(name).Set(l.last.Unix())
	return l
}

// Tick records a completed pass. Called at the END of the work, not the start:
// a loop that begins a pass and blocks forever inside it has not ticked.
func (l *Loop) Tick() {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	l.last = now
	l.mu.Unlock()
	LoopLastTick.With(l.name).Set(now.Unix())
}

// Name is the loop's metric label.
func (l *Loop) Name() string { return l.name }

// overdue reports whether this loop has missed its budget.
func (l *Loop) overdue(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return now.Sub(l.last) > l.budget
}

// Overdue names every loop past its budget, sorted, or nothing when the
// daemon is healthy.
//
// The sort is for the log line: a host restarting under the watchdog should
// say which loop wedged, and two restarts with the same cause should produce
// the same sentence.
func Overdue() []string {
	return overdueAt(time.Now())
}

func overdueAt(now time.Time) []string {
	loopsMu.Lock()
	defer loopsMu.Unlock()
	var out []string
	for name, l := range loops {
		if l.overdue(now) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// resetLoops clears the registry. Tests only.
func resetLoops() {
	loopsMu.Lock()
	defer loopsMu.Unlock()
	loops = map[string]*Loop{}
}
