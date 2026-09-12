package machines

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Whether this host can hold another machine, and what it would free to say
// yes.
//
// # The gap this closes
//
// Nothing on the create path read free memory. A create landed wherever it was
// asked, booted, and either worked or did not; a full host answered a create
// with whatever Firecracker failed with, which is a 500 describing a symptom.
// The doc claimed hosts were "final authority on their own capacity". They
// were not, because nobody asked them.
//
// # Why reclaiming is part of admission and not a separate loop
//
// A host running twenty idle sandboxes is not full. Every one of them will be
// suspended by the idle monitor on its own timer, freeing all of that memory,
// and a create refused in the meantime is refused against a number that was
// about to change. So admission may pull that forward: if the machine fits in
// free memory plus what is reclaimable, the idlest machines are suspended
// until it fits.
//
// This is the same trade a memory-overcommitting hypervisor makes, minus the
// overcommit: nothing is promised twice, the memory is genuinely freed before
// the new machine takes it, and the machines that gave it up wake on their
// next request exactly as they would have.
//
// # What is NOT reclaimable
//
// Suspended machines. Suspend kills the Firecracker process after taking the
// snapshot, so a suspended machine holds no guest memory at all -- its pages
// are already in MemAvailable. Counting it would double-count free memory and
// admit creates that then fail to boot, which is worse than refusing them.

// ErrNoCapacity says this host cannot hold the machine even after reclaiming.
//
// The API package's sentinel rather than one of our own, so the error mapper
// recognises it without this package having to be imported there. A caller's
// response to it is distinct from a failure: a ranker tries the next host, and
// a client that has run out of fleet gets a 507 naming capacity rather than a
// 500 naming whatever the boot failed with.
var ErrNoCapacity = api.ErrNoCapacity

// reclaimGrace is how long a machine must have been quiet before its memory is
// counted as reclaimable.
//
// Shorter than any idle timeout on purpose: this is not "would be suspended
// soon", it is "is doing nothing right now". Long enough that a machine
// between two requests in a burst is not counted and then suspended out from
// under its own traffic.
const reclaimGrace = 30 * time.Second

// Capacity is what this host reports to the fleet each heartbeat.
func (m *Manager) Capacity(ctx context.Context) *state.HostCapacity {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		// A host that cannot read its own machines cannot say what it is
		// holding. Reporting nothing leaves the previous row in place and
		// placement keeps using it, which is stale; reporting zero free would
		// take this host out of the fleet's placement entirely on a blipped
		// read. Stale is the better of the two.
		slog.Warn("could not read machines to report capacity", "err", err)
		return nil
	}
	free := m.freeMemMiB()
	reclaimable, running := m.reclaimable(ctx, rows)
	return &state.HostCapacity{
		MemFreeMiB:        free,
		MemReclaimableMiB: reclaimable,
		CPUCount:          m.opts.CPUCount,
		VCPUsRunning:      running,
		Draining:          m.draining.Load(),
	}
}

// reclaimable sums the memory of this host's RUNNING machines that are idle
// enough to suspend, and the vCPUs in use.
func (m *Manager) reclaimable(ctx context.Context, rows []state.Machine) (memMiB, vcpus int) {
	for _, row := range rows {
		if row.HostID != m.opts.HostID || row.State != StateRunning {
			continue
		}
		vcpus += row.VCPUs
		if m.reclaimableNow(ctx, row) {
			memMiB += row.MemMiB
		}
	}
	return memMiB, vcpus
}

// reclaimableNow reports whether this machine's memory could be taken back
// right now.
//
// Deliberately the idle monitor's own rules, with the WAIT treated as already
// elapsed: everything that makes a machine ineligible for suspend -- auto_stop
// off, a warm floor, a request in flight, being the autoscaler's rather than
// the idle monitor's -- makes it ineligible here too. The only difference is
// that this does not wait out idle_timeout, because the point is to pull that
// suspension forward.
//
// It does not ask the GUEST whether a session is busy, which shouldSuspend
// does last. That is a network round trip per machine, and this runs on every
// heartbeat over every machine on the host; the check is made again, for real,
// at the moment a machine is actually chosen for reclaim.
func (m *Manager) reclaimableNow(ctx context.Context, row state.Machine) bool {
	knobs := ParseKnobs(row.KindKnobs)
	if knobs.AutoStop == "off" {
		return false
	}
	if knobs.MinMachinesRunning > 0 {
		return false
	}
	if m.flight.count(row.ID) > 0 {
		return false
	}
	if time.Since(time.Unix(row.LastActivity, 0)) < reclaimGrace {
		return false
	}
	// A replica of a service's CURRENT release belongs to the autoscaler, not
	// to this host's idea of idleness. Suspending one here would take a
	// replica out of a service the autoscaler is still counting.
	if row.ReleaseID != "" {
		current, err := m.currentRelease(ctx, row.ServiceID)
		if err != nil || current == row.ReleaseID {
			return false
		}
	}
	return true
}

// admit decides whether this host takes a machine of this size, freeing memory
// if that is what it takes.
//
// Returns ErrNoCapacity when it cannot, which is a normal answer and not a
// failure: the caller tries another host, and a client that has run out of
// hosts gets a 507.
func (m *Manager) admit(ctx context.Context, vcpus, memMiB int) error {
	if memMiB <= 0 {
		return nil
	}
	if m.draining.Load() {
		return fmt.Errorf("%w: this host is draining", ErrNoCapacity)
	}
	// vCPUs are timeshared, so a host is not "full" of them the way it is full
	// of memory and there is no reclaim to do. The one thing worth refusing is
	// a machine asking for more CPUs than the host physically has: Firecracker
	// will start it, and it will then contend with itself for ever.
	if m.opts.CPUCount > 0 && vcpus > m.opts.CPUCount {
		return fmt.Errorf("%w: %d vCPUs asked for, this host has %d CPUs",
			ErrNoCapacity, vcpus, m.opts.CPUCount)
	}

	free := m.freeMemMiB()
	if memMiB <= free {
		return nil
	}

	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		// Whether this host can hold the machine is exactly what could not be
		// read. Refusing sends the create to another host, which is a delay;
		// admitting it would boot a machine onto a host that may have no room,
		// which is a failure deep in Firecracker.
		return fmt.Errorf("%w: could not read this host's machines: %w", ErrNoCapacity, err)
	}

	// Idlest first. A machine that has been quiet for an hour is a cheaper
	// thing to suspend than one quiet for a minute, and suspending in that
	// order means the fewest woken again shortly after.
	var candidates []state.Machine
	for _, row := range rows {
		if row.HostID != m.opts.HostID || row.State != StateRunning {
			continue
		}
		if m.reclaimableNow(ctx, row) {
			candidates = append(candidates, row)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastActivity < candidates[j].LastActivity
	})

	reclaimable := 0
	for _, row := range candidates {
		reclaimable += row.MemMiB
	}
	if memMiB > free+reclaimable {
		return fmt.Errorf("%w: %d MiB asked for, %d free and %d reclaimable",
			ErrNoCapacity, memMiB, free, reclaimable)
	}

	// Suspend only as many as it takes. Freeing everything reclaimable
	// whenever one machine needs room would turn one create into a fleet-wide
	// cold start.
	freed := 0
	for _, row := range candidates {
		if memMiB <= free+freed {
			break
		}
		if err := m.Suspend(ctx, row.ID); err != nil {
			// One machine that would not suspend is not the end of the
			// attempt: the next one may free enough. It is logged because a
			// machine that repeatedly refuses to suspend is a real fault.
			slog.Warn("could not reclaim a machine's memory for an incoming create",
				"machine", row.ID, "err", err)
			continue
		}
		freed += row.MemMiB
		slog.Info("suspended an idle machine to make room",
			"machine", row.ID, "freed_mib", row.MemMiB, "needed_mib", memMiB)
	}

	// Read the real figure again rather than trusting the arithmetic: the
	// suspends actually happened, and what matters is what the kernel now
	// says, not what this function expected them to free.
	if memMiB > m.freeMemMiB() {
		return fmt.Errorf("%w: reclaimed %d MiB and still short of %d",
			ErrNoCapacity, freed, memMiB)
	}
	return nil
}

// freeMemMiB is what the host says it has, or "unknown" when it cannot say.
//
// Unknown is a large number rather than zero on purpose. A host that cannot
// measure its own memory must not refuse every create: the old behaviour --
// admit and let the boot decide -- is worse than a wrong refusal only in that
// it fails later, and refusing everything would take the host out of the fleet
// on a blipped read of /proc/meminfo.
func (m *Manager) freeMemMiB() int {
	if m.opts.FreeMemMiB == nil {
		return unknownFreeMem
	}
	return m.opts.FreeMemMiB()
}

// unknownFreeMem is what a host that cannot measure itself reports: enough
// that admission never refuses, which is the behaviour every host had before
// admission existed.
const unknownFreeMem = 1 << 30

// SetDraining marks this host as taking no new machines. Placement skips a
// draining host, which is what lets a drain converge instead of racing the
// placer for the machines it is moving off.
func (m *Manager) SetDraining(on bool) { m.draining.Store(on) }

// Draining reports the flag.
func (m *Manager) Draining() bool { return m.draining.Load() }
