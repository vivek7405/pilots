// Package selfheal returns a dead host's machines to the fleet.
//
// No leader, no election, no scheduler. Every host heartbeats its own row and
// independently computes the same answer to "which of the missing machines are
// mine to rescue" -- from the sorted list of live hosts and a fixed hash. Two
// hosts that agree on those two things cannot both rescue the same machine,
// and cannot both decide it is someone else's.
package selfheal

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

const (
	// HeartbeatInterval is how often a host says it is alive.
	HeartbeatInterval = 5 * time.Second

	// DeadAfter is how long silence means gone. Six missed heartbeats: long
	// enough that a busy host or a gossip hiccup is not mistaken for a dead
	// one, short enough that a real failure is noticed in well under a minute.
	//
	// It MUST match the store's own threshold, which re-checks liveness at the
	// moment of the claim. A rescuer using a shorter window than the store
	// would have its claims refused; a longer one and the store would accept
	// claims this loop should never have made.
	DeadAfter = 30 * time.Second

	// RescueInterval is how often the fleet is checked for orphans.
	RescueInterval = 10 * time.Second
)

// Fleet is the cluster's current view. Backed by the subscription cache, so
// these are map reads rather than queries.
type Fleet interface {
	Machines() []state.Machine
	LiveHosts(now time.Time, deadAfter time.Duration) []state.Host
	// MachineVendor names the vendor pool a machine's memory image belongs to,
	// or "" when nothing has recorded one. It narrows the rescue candidate set
	// -- a snapshot never restores across the Intel/AMD boundary -- and an
	// empty answer ranks over the whole fleet, which is what a machine that
	// predates the table gets.
	MachineVendor(id string) string
}

// Options wires the loops to the host they run on.
type Options struct {
	HostID string
	Fleet  Fleet
	Store  state.Store

	// Ready reports whether this host's replica has caught up with the fleet.
	// Nil means always ready, which is the single box and every test that is
	// not about the gate.
	//
	// This loop is the reason the join gate exists. Its whole job is to act on
	// machines whose owner it cannot see, and on a half-replicated replica an
	// owner that is merely unseen is indistinguishable from one that is dead:
	// both are an empty result. The claim that follows merges cleanly into a
	// row a live host is still writing, so nothing errors and two hosts end up
	// believing they own the same machine.
	Ready func() bool

	// Capacity reports whether this host can take another machine of this
	// size. Refusing is normal and costs nothing: the next tick recomputes the
	// live set and re-hashes, so a full host does not wedge a machine.
	Capacity func(vcpus, memMiB int) bool

	// Restore brings a rescued machine up here. It receives the row and
	// nothing else -- everything host-local is minted fresh.
	Restore func(ctx context.Context, m *state.Machine) error

	// RunningLocally lists the machines this host currently has processes for.
	RunningLocally func() []string
	// StopLocal tears down a machine this host is running but no longer owns.
	StopLocal func(ctx context.Context, id string) error

	// Heartbeat reports this host's identity and free capacity.
	Heartbeat func() state.Host

	// Capacities reports what this host can still hold, written beside the
	// heartbeat so placement reads a figure as fresh as liveness itself. Nil
	// on a host that publishes none, which is every host in a test that is not
	// about placement.
	//
	// Separate from Heartbeat because the two rows have different jobs: the
	// hosts row is the liveness signal every survivor reads to decide who is
	// dead, and capacity is advisory. Folding capacity into the hosts row
	// would also mean a column add on a table that has rows (rule 6).
	Capacities func() *state.HostCapacity

	// CachedBuilds reports the build ids this host holds on local disk, or nil
	// when nothing has changed since the last tick.
	//
	// Nil is the common answer, and that is the point: this row is gossiped in
	// FULL on every write, and a build cache changes far more often than
	// placement needs to hear about it. Writing it every five seconds would
	// starve the apply loop for every other row on the fleet.
	CachedBuilds func() []string

	// Now is overridable for tests.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// RunHeartbeat writes this host's row until ctx is done.
//
// The ONLY writer of this row. It is what every other host's liveness check
// reads, so a host that stops writing it is, by definition, gone -- and its
// machines become someone else's to rescue.
func RunHeartbeat(ctx context.Context, opts Options) {
	live := metrics.NewLoop("heartbeat", 3*HeartbeatInterval)
	tick := time.NewTicker(HeartbeatInterval)
	defer tick.Stop()

	for {
		h := opts.Heartbeat()
		h.ID = opts.HostID
		h.LastSeen = opts.now().Unix()

		if err := opts.Store.PutHost(ctx, &h); err != nil && ctx.Err() == nil {
			// Worth shouting about: a host that cannot heartbeat is about to
			// have its machines rescued out from under it.
			slog.Error("could not write this host's heartbeat; the fleet will "+
				"shortly treat this host as dead", "err", err)
		}
		// Capacity and the cached-build set ride the same tick, AFTER the
		// hosts row: a capacity row for a host the fleet does not yet believe
		// in is a row no ranker will read, and writing it first would only
		// widen that window.
		//
		// Neither failure is fatal here. A host that cannot publish its
		// capacity is simply not preferred by placement, which is the safe
		// direction; a host that cannot heartbeat at all is the serious case,
		// and it is shouted about above.
		if opts.Capacities != nil {
			if c := opts.Capacities(); c != nil {
				c.HostID = opts.HostID
				c.UpdatedAt = opts.now().Unix()
				if err := opts.Store.PutHostCapacity(ctx, c); err != nil && ctx.Err() == nil {
					slog.Warn("could not publish this host's capacity; placement will "+
						"not prefer it until this clears", "err", err)
				}
			}
		}
		if opts.CachedBuilds != nil {
			if ids := opts.CachedBuilds(); ids != nil {
				if err := opts.Store.PutHostBuilds(ctx, &state.HostBuilds{
					HostID: opts.HostID, Builds: ids, UpdatedAt: opts.now().Unix(),
				}); err != nil && ctx.Err() == nil {
					slog.Warn("could not publish this host's cached builds; placement "+
						"loses only the affinity bonus", "err", err)
				}
			}
		}

		// Ticked even when the write failed: what this watches is whether the
		// LOOP is running. A store that refuses is loud already, in the line
		// above and in every peer's view of this host. A loop that stopped
		// spinning says nothing at all, which is the case the watchdog is for.
		live.Tick()

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// RunRescue reclaims orphaned machines until ctx is done.
func RunRescue(ctx context.Context, opts Options) {
	live := metrics.NewLoop("self_heal", 3*RescueInterval)
	tick := time.NewTicker(RescueInterval)
	defer tick.Stop()

	for {
		Tick(ctx, opts)
		live.Tick()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Tick runs one pass. Exported so a test can drive it deterministically.
func Tick(ctx context.Context, opts Options) {
	// Shutting down what we lost comes FIRST, before claiming anything. A host
	// that lost a claim while it was partitioned is serving a machine someone
	// else now owns, and every moment it keeps serving is a moment two
	// Firecrackers are writing the same machine's disk.
	releaseLost(ctx, opts)

	// Releasing what we lost is a read of PRESENCE: another host's id is in
	// the row, which a partial replica can only under-report, never invent. So
	// it runs before the gate. Everything below this line reads an ABSENCE and
	// waits.
	if opts.Ready != nil && !opts.Ready() {
		slog.Warn("replication not complete; claiming nothing this tick")
		return
	}

	now := opts.now()

	// Sorted HERE, not trusted from the caller.
	//
	// Rank is a position in this list, so two hosts that order it differently
	// compute different ranks -- and then either both rescue a machine or
	// neither does. The cache already returns it sorted; sorting again makes
	// this loop's correctness independent of that, because the cost of the
	// coupling being broken later is silent and severe.
	live := sortedByID(opts.Fleet.LiveHosts(now, DeadAfter))

	rank := rankOf(live, opts.HostID)
	if rank < 0 {
		// Our own heartbeat is stale, so we are the host others are about to
		// rescue from. Claiming anything now would be a sick host taking on
		// more work, and the claims would be refused anyway.
		slog.Warn("this host's own heartbeat is stale; rescuing nothing this tick")
		return
	}

	liveIDs := make(map[string]bool, len(live))
	for _, h := range live {
		liveIDs[h.ID] = true
	}

	for _, m := range opts.Fleet.Machines() {
		if liveIDs[m.HostID] || m.State == state.StateDestroyed {
			continue
		}
		if rescuer, ok := RescuerFor(m.ID, opts.Fleet.MachineVendor(m.ID), live); !ok || rescuer != opts.HostID {
			continue
		}
		if opts.Capacity != nil && !opts.Capacity(m.VCPUs, m.MemMiB) {
			// Not a retry-in-place: the next tick recomputes the live set and
			// re-hashes, so a full host does not wedge a machine forever.
			slog.Info("no capacity to rescue a machine; leaving it for the next tick",
				"machine", m.ID)
			continue
		}
		rescue(ctx, opts, m)
	}
}

// releaseLost stops machines this host runs but no longer owns.
//
// THE most important invariant in the phase. Merges are per column and
// last-write-wins, so a partitioned host and its rescuer can briefly disagree
// about who owns a machine -- but only one of them may have a Firecracker
// serving it. The row is the authority, and a host that finds itself not named
// by it shuts down, immediately, without arguing.
//
// Without this, a host that comes back from a partition keeps serving a
// machine another host has already restored: two VMs, one disk in object
// storage, and whichever writes last wins.
func releaseLost(ctx context.Context, opts Options) {
	if opts.RunningLocally == nil || opts.StopLocal == nil {
		return
	}

	for _, id := range opts.RunningLocally() {
		row, err := opts.Store.GetMachine(ctx, id)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// Destroyed elsewhere while we were away.
				slog.Warn("stopping a machine whose row is gone", "machine", id)
				if serr := opts.StopLocal(ctx, id); serr != nil {
					slog.Error("could not stop it", "machine", id, "err", serr)
				}
			}
			continue
		}
		if row.HostID == opts.HostID {
			continue
		}

		slog.Warn("this host is running a machine it no longer owns; stopping it "+
			"before two hosts serve the same machine",
			"machine", id, "owner", row.HostID)
		if err := opts.StopLocal(ctx, id); err != nil {
			slog.Error("could not stop a machine this host lost",
				"machine", id, "err", err)
		}
	}
}

// rescue claims one machine and brings it up here.
func rescue(ctx context.Context, opts Options, m state.Machine) {
	// A machine with no memory image cannot be restored anywhere: it was never
	// suspended or checkpointed, so there is nothing in object storage to
	// bring back. Looping on it every tick forever helps nobody.
	if m.MemBuildID == "" {
		// Nothing is written to the row. Claiming a machine this host cannot
		// restore would be destruction, not rescue: the owner may only be
		// partitioned, still running the machine -- a claim here makes the
		// returning owner kill its own healthy VM, with nothing anywhere to
		// bring back. Logged once per machine instead of every tick.
		if _, seen := unrescuable.LoadOrStore(m.ID, struct{}{}); !seen {
			slog.Error("a machine cannot be rescued: it has no snapshot in object "+
				"storage, so its state did not survive its host",
				"machine", m.ID, "dead_host", m.HostID)
		}
		return
	}

	slog.Info("rescuing a machine from a host that stopped responding",
		"machine", m.ID, "dead_host", m.HostID)

	// Restore OWNS the claim, and this loop must not make one of its own.
	//
	// Claiming here as well means the second claim -- the one inside Restore
	// -- reads a row that now names THIS host and refuses it, because this
	// host is very much alive. The machine ends up claimed, unrestored, and
	// marked error, with a message about refusing to claim from a host that
	// is still heartbeating: itself.
	//
	// The claim belongs with the restore because they must happen under the
	// same per-machine lock: between claiming and starting the machine,
	// nothing else may decide the machine is free.
	if err := opts.Restore(ctx, &m); err != nil {
		// Losing the claim is ordinary -- another survivor got there first,
		// or the owner came back -- and so is a restore that failed. Restore
		// records the outcome on the row; the next tick re-reads the fleet.
		slog.Info("did not rescue a machine", "machine", m.ID, "err", err)
		return
	}
	slog.Info("machine rescued", "machine", m.ID, "url", m.Domain)
}

// unrescuable remembers machines this process has already given up on, so the
// rescue loop logs each of them once rather than on every tick. In-memory on
// purpose: it suppresses a log line, never a rescue -- a machine that later
// gains a snapshot takes the rescue path and never consults this set.
var unrescuable sync.Map

// sortedByID puts the live set in the one order every host must agree on.
func sortedByID(hosts []state.Host) []state.Host {
	out := append([]state.Host(nil), hosts...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rankOf is this host's position in the live set, or -1 if it is not in it.
func rankOf(live []state.Host, hostID string) int {
	for i, h := range live {
		if h.ID == hostID {
			return i
		}
	}
	return -1
}

// mine reports whether a machine falls in this host's slice.
//
// FNV-1a by name, not a map hash. Go's built-in hashes are seeded per process,
// so two hosts running identical code would compute different buckets for the
// same machine -- and then either both rescue it or neither does. The whole
// scheme rests on every survivor computing the same answer from the same
// inputs, so the function has to be one that cannot vary.
// RescuerFor names the ONE host responsible for rescuing a machine.
//
// Exported because the rescue loop is no longer the only caller: a request
// arriving at a survivor for a machine whose host is gone rescues it on the
// spot, and that path has to reach the same answer this loop does. Two
// different answers means two hosts claiming one machine -- and a claim
// checks a local CRDT replica, which cannot exclude anything, so both would
// succeed and both would start a Firecracker on the same machine's state.
//
// It sorts its own input for the same reason Tick does: rank is a position in
// a list, and two hosts that order it differently compute different owners.
func RescuerFor(machineID, vendor string, live []state.Host) (string, bool) {
	// One implementation, because the service arbiter in the corrosion store
	// has to compute owners the same way this does. Two copies of a
	// consensus-free ownership rule is two chances to disagree. The vendor only
	// narrows the candidate set; the hash and the sort are OwnerFor's.
	return state.MachineOwnerFor(machineID, vendor, live)
}

func hashID(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// SliceOf reports which rank owns a machine, for diagnostics.
func SliceOf(machineID string, liveCount int) int {
	if liveCount <= 0 {
		return -1
	}
	return int(hashID(machineID) % uint32(liveCount))
}

// SortedLiveIDs is the ordering every host must agree on.
//
// Exported so a test can assert the property directly: rank comes from
// position in this list, so two hosts that order it differently compute
// different ranks and the slices stop partitioning the machines.
func SortedLiveIDs(live []state.Host) []string {
	ids := make([]string, len(live))
	for i, h := range live {
		ids[i] = h.ID
	}
	sort.Strings(ids)
	return ids
}
