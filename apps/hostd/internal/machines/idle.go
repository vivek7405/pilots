package machines

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// DefaultIdleTimeout is how long a machine must be quiet before it suspends
// when its knobs say nothing else; the idle_timeout knob is the per-machine
// value.
const DefaultIdleTimeout = api.DefaultIdleTimeoutSeconds * time.Second

// builderMaxSuspended is how long a suspended builder is kept before it is
// destroyed.
//
// A suspended builder is not free: it holds a memory image and a 32 GiB disk
// image in object storage and on this host's NVMe. Keeping one for a day
// covers a working day of deploys, so a developer never pays a create; past
// that the next build pays one create, which sits inside the build's own
// timeout and is invisible next to the solve.
//
// fly terminates its builder after ten minutes of inactivity, which is the
// opposite trade: cheaper to hold, slower on the next deploy. A restore here
// is sub-second, so the balance is different.
const builderMaxSuspended = 24 * time.Hour

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

// acquire takes a slot on a machine that has a hard limit, waiting briefly for
// room rather than refusing the instant it is full.
//
// The wait is the difference between a limit and a cliff. A burst that crosses
// the line for two hundred milliseconds should be served two hundred
// milliseconds late, not refused: the autoscaler is already starting another
// replica, and a request that waits is a request that succeeds. What the limit
// exists to prevent is the queue growing without bound, which is why the wait
// is short and ends in a refusal rather than in a longer wait.
//
// Reports false when the deadline passes or the caller goes away, and takes no
// slot in that case.
func (f *inFlight) acquire(ctx context.Context, id string, limit int, wait time.Duration) bool {
	if limit <= 0 {
		f.begin(id)
		return true
	}
	deadline := time.Now().Add(wait)
	for {
		f.mu.Lock()
		if f.n[id] < limit {
			f.n[id]++
			f.mu.Unlock()
			return true
		}
		f.mu.Unlock()

		// Polled rather than signalled by a condition variable. end() is on
		// the hot path of every request the router serves, and making it wake
		// waiters would put a lock handoff there for a case that is rare by
		// construction: a machine at its hard limit is already the exception.
		// The cost of polling is one wakeup every few milliseconds on exactly
		// the requests that are already waiting.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		step := 5 * time.Millisecond
		if remaining < step {
			step = remaining
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(step):
		}
	}
}

// BeginLimited takes a slot on a machine, respecting its hard limit. False
// means the machine is full and the caller must refuse.
func (m *Manager) BeginLimited(ctx context.Context, id string, limit int, wait time.Duration) bool {
	return m.flight.acquire(ctx, id, limit, wait)
}

// total is every machine's in-flight count summed, for pilots_router_inflight.
// Summed here rather than published per machine: a series per machine is
// exactly the cardinality the metrics package doc refuses.
func (f *inFlight) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.n {
		n += c
	}
	return n
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
	live := metrics.NewLoop("idle_monitor", 3*idleCheckInterval)
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.suspendIdleMachines(ctx)
			live.Tick()
		}
	}
}

func (m *Manager) suspendIdleMachines(ctx context.Context) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		slog.Error("idle monitor could not list machines", "err", err)
		return
	}
	// Published from the tick, not from the scrape: this is the one loop that
	// already lists the host's rows, and a scrape must never query the store.
	m.countByState(rows)

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

	// Reuses the rows this tick already listed. A second ListMachines here
	// would double the only store read the monitor makes.
	m.destroyStaleBuilders(ctx, rows)

	// Scheduled volume snapshots ride this loop rather than a ticker of their
	// own. It already runs every few seconds over this host's state, and a
	// second timer would be a second thing to keep alive and a second thing to
	// notice when it stops.
	m.snapshotDueVolumes(ctx)
}

// destroyStaleBuilders collects builders that have been suspended longer than
// builderMaxSuspended.
//
// A builder is hostd's machine, not the org's, so nothing else will ever clean
// one up: an org that deploys once and never again would otherwise leave a
// memory image and a disk image behind on every host it ever built on. The
// next build after a collection simply creates one again.
func (m *Manager) destroyStaleBuilders(ctx context.Context, rows []state.Machine) {
	for _, id := range m.selectStaleBuilders(rows) {
		if err := m.Destroy(ctx, id); err != nil {
			slog.Error("could not destroy a stale builder", "machine", id, "err", err)
			continue
		}
		slog.Info("destroyed a builder that had been suspended for a day", "machine", id)
	}
}

// selectStaleBuilders is the choice destroyStaleBuilders acts on, split out so
// it can be asserted without an engine behind it.
func (m *Manager) selectStaleBuilders(rows []state.Machine) []string {
	var stale []string
	for _, row := range rows {
		// Single-writer: only the owning host may write this row. A RUNNING
		// builder is never collected either, because the idle monitor
		// suspends it first and a build in flight keeps it running.
		if row.HostID != m.opts.HostID || row.State != StateSuspended {
			continue
		}
		if !strings.HasPrefix(row.Name, builderNamePrefix) {
			continue
		}
		// LastActivity is stamped by Touch, which EnsureBuilder calls when it
		// hands a builder out and again when the build releases it. So this
		// measures time since the last BUILD, not time since the suspend.
		if time.Since(time.Unix(row.LastActivity, 0)) < builderMaxSuspended {
			continue
		}
		stale = append(stale, row.ID)
	}
	return stale
}

// shouldSuspend requires BOTH signals to agree: nothing in flight, and no
// activity for the timeout.
//
// Concurrency alone would suspend a machine between two requests. The timer
// alone would suspend one that is busy but generating no HTTP traffic. Only
// the conjunction is safe.
func (m *Manager) shouldSuspend(ctx context.Context, row state.Machine) bool {
	// A machine being handed to another host is nobody's to suspend: the drain
	// already suspended it, or is about to, and a second suspend racing the
	// handoff would write a row the target is in the middle of claiming.
	if _, moving := m.HandingOff(row.ID); moving {
		return false
	}

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

	// The wait is the machine's own. A blob stored before the knob existed
	// has no idle_timeout and reads as the default through ParseKnobs; a
	// zero written by an older test fixture is treated the same way rather
	// than as "suspend the instant it goes quiet".
	timeout := DefaultIdleTimeout
	if knobs.IdleTimeout > 0 {
		timeout = time.Duration(knobs.IdleTimeout) * time.Second
	}
	idleFor := time.Since(time.Unix(row.LastActivity, 0))
	if idleFor < timeout {
		return false
	}

	// Last, and only for a machine every signal above has already agreed to
	// suspend: ask the guest whether a console session is still running a
	// command. A client that detached took hostd's only view of that session
	// with it; the guest's process tree is the view that remains. This is the
	// one step that talks to the guest, which is why it is not the first.
	if slot, ok := m.SlotFor(row.ID); ok && m.sessionsBusy(ctx, row.ID, slot.AgentAddr()) {
		// A running command is activity, and activity restarts the wait:
		// without this touch the machine would suspend on the first tick
		// after the command ended, not idle_timeout later as promised.
		m.Touch(ctx, row.ID)
		return false
	}
	return true
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
