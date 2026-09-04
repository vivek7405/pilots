package machines

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// DefaultIdleTimeout is how long a machine must be quiet before it suspends.
const DefaultIdleTimeout = 60 * time.Second

// idleCheckInterval is how often the monitor looks. Frequent enough that a
// machine suspends promptly, cheap because it is a local read.
const idleCheckInterval = 10 * time.Second

// inFlight counts requests currently being served per machine.
//
// This is the second half of the idle decision, and it is the half that stops
// a machine being suspended out from under a long-running request.
type inFlight struct {
	mu sync.Mutex
	n  map[string]int
}

func newInFlight() *inFlight { return &inFlight{n: make(map[string]int)} }

func (f *inFlight) begin(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n[id]++
}

func (f *inFlight) end(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[id] > 0 {
		f.n[id]--
	}
	if f.n[id] == 0 {
		delete(f.n, id)
	}
}

func (f *inFlight) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[id]
}

// Begin and End bracket a request against a machine, and Touch records
// activity that is not a request -- an exec, a websocket frame.
func (m *Manager) Begin(id string) { m.flight.begin(id) }
func (m *Manager) End(id string)   { m.flight.end(id) }

// Touch marks a machine as recently used.
//
// Called from both the router and every exec, because an agent building
// something inside a machine generates no HTTP traffic at all -- suspending it
// mid-build because "nobody visited the URL" would be indefensible.
func (m *Manager) Touch(ctx context.Context, id string) {
	// A narrow write, not a read-modify-write of the whole row.
	//
	// Upserting every column raced Suspend and Wake, which write the same row
	// under the machine's lock: read the row while it said running, have
	// Suspend commit, then write the stale copy back, and the row claimed
	// running for a machine that was suspended and already dropped. That
	// wedges the URL for good, because every repair path trusts the row --
	// the router sees running and never wakes it, and the idle monitor's
	// Suspend returns early for a machine it no longer holds.
	//
	// Whole-row upserts also become last-writer-wins merges under Corrosion,
	// so the narrow write is what keeps working when the store is replicated.
	_ = m.opts.Store.TouchMachine(ctx, id, time.Now().Unix())
}

// RunIdleMonitor suspends machines that have gone quiet, until ctx ends.
func (m *Manager) RunIdleMonitor(ctx context.Context) {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.suspendIdleMachines(ctx)
		}
	}
}

func (m *Manager) suspendIdleMachines(ctx context.Context) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		slog.Error("idle monitor could not list machines", "err", err)
		return
	}

	for _, row := range rows {
		if row.HostID != m.opts.HostID || row.State != StateRunning {
			continue
		}
		if !m.shouldSuspend(ctx, row) {
			continue
		}
		if err := m.Suspend(ctx, row.ID); err != nil {
			slog.Error("idle suspend failed", "machine", row.ID, "err", err)
			continue
		}
		slog.Info("machine suspended after going idle", "machine", row.ID)
	}
}

// shouldSuspend requires BOTH signals to agree: nothing in flight, and no
// activity for the timeout.
//
// Concurrency alone would suspend a machine between two requests. The timer
// alone would suspend one that is busy but generating no HTTP traffic. Only
// the conjunction is safe.
func (m *Manager) shouldSuspend(ctx context.Context, row state.Machine) bool {
	// Whose machine is this? Every running machine needs exactly one
	// controller: two would race, none bills forever.
	//
	// A replica of the service's CURRENT release is the autoscaler's. It
	// reads the same knobs plus the fleet-wide floor and gives the replica
	// back to its owner host, so the idle monitor steps aside. Keyed on the
	// release rather than a knob: a promoted sandbox keeps its sandbox knobs
	// and changes owner the moment it gets a release, and a rollout's replica
	// is the same. Suspend keeps a slot and the tenant filter writes a wake
	// rule on the same key.
	//
	// A replica of a SUPERSEDED release is nobody's. The autoscaler only ever
	// enumerates the current release's replicas, so stepping aside for every
	// release id at all left these with no controller. Deploy and Rollback do
	// suspend them, but best-effort: one failed Suspend -- a transient lock, a
	// jailer hiccup -- and a Firecracker runs and bills forever with nothing
	// that will ever reconsider it. They stay the idle monitor's.
	superseded := false
	if row.ReleaseID != "" {
		current, err := m.currentRelease(ctx, row.ServiceID)
		if err != nil {
			// Which controller owns it is exactly what could not be read, so
			// take the reversible side: a machine left running until the next
			// tick reads the row is recoverable, a second controller racing
			// the autoscaler on a live replica is not.
			slog.Warn("idle monitor could not tell whether a replica is current; leaving it alone",
				"machine", row.ID, "service", row.ServiceID, "err", err)
			return false
		}
		if current == row.ReleaseID {
			return false
		}
		superseded = true
	}

	knobs := ParseKnobs(row.KindKnobs)

	if knobs.AutoStop == "off" {
		return false
	}
	// A sandbox asked to keep a floor is not a scale-to-zero candidate.
	//
	// A floor is a property of a release's replica SET, though, and a
	// superseded release has no set left to keep warm -- its traffic went to
	// the new release the moment the service row flipped. Honouring the floor
	// there would re-open the leak for every warm service, which is the
	// common case for the workloads that set a floor at all.
	if knobs.MinMachinesRunning > 0 && !superseded {
		return false
	}
	if m.flight.count(row.ID) > 0 {
		return false
	}

	idleFor := time.Since(time.Unix(row.LastActivity, 0))
	return idleFor >= DefaultIdleTimeout
}

// currentRelease is the release a service is serving right now, or "" when
// there is no service to ask.
//
// A local point read on a row this host already holds, once per running
// replica per tick -- the same order of cost as the ListMachines above it,
// and on the same local-state-only path the routing rules demand.
//
// A machine that names a release but no service, or names a service that no
// longer exists, is not the autoscaler's either: nothing enumerates it. Both
// answer "" so it falls through to the idle monitor rather than to nobody.
func (m *Manager) currentRelease(ctx context.Context, serviceID string) (string, error) {
	if serviceID == "" {
		return "", nil
	}
	svc, err := m.opts.Store.GetService(ctx, serviceID)
	if errors.Is(err, state.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return svc.ReleaseID, nil
}
