package machines

import (
	"context"
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
	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return
	}
	row.LastActivity = time.Now().Unix()
	row.UpdatedAt = time.Now().Unix()
	_ = m.opts.Store.PutMachine(ctx, row)
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
		if !m.shouldSuspend(row) {
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
func (m *Manager) shouldSuspend(row state.Machine) bool {
	knobs := ParseKnobs(row.KindKnobs)

	if knobs.AutoStop == "off" {
		return false
	}
	// A service with a floor of running instances is not a scale-to-zero
	// candidate. Phase 5 makes this per-service rather than per-machine.
	if knobs.MinMachinesRunning > 0 {
		return false
	}
	if m.flight.count(row.ID) > 0 {
		return false
	}

	idleFor := time.Since(time.Unix(row.LastActivity, 0))
	return idleFor >= DefaultIdleTimeout
}
