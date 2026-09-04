package main

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// guestLoad is what the autoscaler learns from the root namespace: which
// replicas have open guest-to-guest sessions right now.
//
// It starts BLIND, and goes blind again whenever the read fails. This is the
// one signal that stops a replica being suspended out from under a live
// session -- an idle-looking Postgres with an open transaction on it -- and a
// stale or absent reading of it is indistinguishable from "nobody is
// connected". Reporting zero there is the mid-transaction kill this whole
// half of the feature exists to prevent, so it is never the answer given for
// a reading nobody has.
type guestLoad struct {
	mu   sync.Mutex
	held map[string]int
	// blind is set until the first successful read and again after every
	// failed one, because the map then describes a moment that has passed.
	blind bool
}

func newGuestLoad() *guestLoad {
	// Blind until something has actually been read: the autoscaler may tick
	// before the first pass lands.
	metrics.SessionSignalBlind.Set(1)
	return &guestLoad{blind: true}
}

// Held is open sessions to a machine, or 1 when that cannot be read.
//
// Fail SAFE, deliberately, and it is a real trade: while the read is failing
// no replica on this host is given back, so a permanently unreadable
// conntrack table costs money until someone fixes it. It costs money loudly
// -- Warn plus pilots_session_signal_blind -- where the other choice loses
// somebody's transaction quietly. Only local replicas are ever asked (a flow
// is visible only in the namespace it crosses), so this holds this host's own
// replicas and says nothing about anyone else's.
func (g *guestLoad) Held(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blind {
		return 1
	}
	return g.held[id]
}

func (g *guestLoad) set(held map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held, g.blind = held, false
	metrics.SessionSignalBlind.Set(0)
}

// unreadable drops the last reading rather than keeping it.
//
// Keeping it sticks in whichever direction the last good pass happened to
// land: a pass that saw sessions would hold the replica up forever, one that
// saw none would let it be suspended forever. Neither is a reading, so it
// stops being treated as one.
func (g *guestLoad) unreadable() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held, g.blind = nil, true
	metrics.SessionSignalBlind.Set(1)
}

// load joins the router's in-flight count with the namespace's open sessions.
// It is the autoscaler's whole view of a replica.
type load struct {
	*machines.Manager
	guest *guestLoad
}

func (l load) Held(id string) int { return l.guest.Held(id) }

type machineToucher interface {
	Touch(ctx context.Context, id string)
}

// runActivity turns root-namespace traffic into the two signals the
// autoscaler reads: a rising packet count on a running replica's address is a
// Touch on its row, and an established session to it is a held count.
//
// Same tick as the waker, for the same reason: the rules and the counts are
// two halves of one mechanism.
func runActivity(ctx context.Context, view fleetView, loc *mesh.Locator,
	toucher machineToucher, guest *guestLoad) {

	tick := time.NewTicker(wakeInterval)
	defer tick.Stop()

	// One gate each, because the two reads fail for different reasons and an
	// operator needs to know which.
	var counters, sessions errGate
	seen := map[string]uint64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		if counts, err := netns.ActivityCounts(); err != nil {
			counters.failed("could not read activity counters", err)
		} else {
			counters.recovered()
			for _, id := range risen(seen, counts) {
				toucher.Touch(ctx, id)
			}
		}

		flows, err := netns.OpenFlows()
		if err != nil {
			// Warn, not Debug: this is the signal that keeps a live session
			// from being suspended, and at Debug it disappears on every
			// production host. The held map is dropped in the same breath, so
			// the autoscaler is told the signal is missing rather than told
			// there are no sessions.
			sessions.failed("could not list open guest-to-guest sessions; "+
				"treating every local replica as busy until this recovers", err)
			guest.unreadable()
			continue
		}
		sessions.recovered()
		running := map[netip.Addr]string{}
		for _, m := range view.Machines() {
			if m.State != "running" {
				continue
			}
			if addr, ok := loc.MachineAddress(m); ok {
				running[addr] = m.ID
			}
		}
		guest.set(netns.HeldBy(flows, running))
	}
}

// risen reports the machines whose count rose since the last reading and
// records the new baselines. A machine seen for the first time only sets its
// baseline: the table is rebuilt, and its counters zeroed, whenever the fleet
// changes, and a reset must not read as traffic.
func risen(seen map[string]uint64, counts map[string]uint64) []string {
	var out []string
	for id, n := range counts {
		last, known := seen[id]
		seen[id] = n
		if known && n > last {
			out = append(out, id)
		}
	}
	for id := range seen {
		if _, still := counts[id]; !still {
			delete(seen, id)
		}
	}
	return out
}

// errGate keeps a repeating failure loud without making it spam.
//
// The loop ticks every couple of seconds, so warning on each pass buries the
// log and warning once buries the failure. It warns on the way in, once every
// warnEvery passes while it lasts, and once on the way out -- so a
// still-broken host keeps saying so, and the recovery is in the log too.
type errGate struct {
	failing bool
	n       int
}

// warnEvery is how many consecutive failures pass between warnings, chosen
// against wakeInterval to land near once a minute.
const warnEvery = 30

func (g *errGate) failed(msg string, err error) {
	metrics.ActivityReadErrors.Inc()
	g.n++
	if !g.failing || g.n%warnEvery == 0 {
		slog.Warn(msg, "err", err, "consecutive", g.n)
	}
	g.failing = true
}

func (g *errGate) recovered() {
	if g.failing {
		slog.Warn("root namespace reads recovered", "after", g.n)
	}
	g.failing, g.n = false, 0
}
