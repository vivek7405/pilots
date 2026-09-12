package state

import (
	"sort"
	"time"
)

// Where a NEW machine goes.
//
// # Why this is a different question from self-heal
//
// Self-heal keeps the hash (MachineOwnerFor): it runs when a host has died, so
// there is nobody to ask and a deterministic slice is the only way survivors
// agree on who rescues what without electing anyone.
//
// A create is not that. The whole live fleet is available, every host already
// holds every other host's capacity in its local replica, and putting a new
// machine on a host that cannot hold it is a failure the fleet could have
// avoided by looking. So a create RANKS.
//
// # Why ranking needs no coordinator
//
// The ranker only proposes. The target disposes: it admits the machine against
// its own free memory, or refuses, and the ranker moves to the next candidate.
// Two hosts ranking two creates at the same moment from slightly different
// replicas is therefore not a race to prevent -- it is the design. A stale
// ranking costs one forward and a refusal, never a double-booked host.
//
// This is what keeps the no-control-plane rule intact while still making a
// decision better than a hash: the decision is advisory, and the authority
// stays with the host that owns the resource.
//
// # Why spread and not pack
//
// Highest headroom AFTER placement wins. Bin-packing a fleet of microVMs is
// how a platform arrives at every host running at 95% with no room to absorb
// the next burst, and the burst is exactly when the capacity is needed. The
// cost of spreading is some hosts idle; the cost of packing is creates that
// fail.

// PlacementRequest is what a create needs, as the ranker sees it.
type PlacementRequest struct {
	// Name is the machine's name, used only as the tie-break. Keeping the hash
	// here means an exact tie resolves the same way on every host, so the
	// ranking does not depend on the order rows happened to arrive in.
	Name string
	// VCPUs and MemMiB are the size the machine will be.
	VCPUs  int
	MemMiB int
	// Vendor, when set, restricts the candidates to hosts of one CPU vendor.
	// A Firecracker memory image carries raw CPUID and never restores across
	// the Intel/AMD boundary, so a create that RESTORES an image must land in
	// its pool. A create that boots leaves this empty and may go anywhere.
	Vendor string
	// Builds are the build ids this create needs on disk. A host that already
	// has them is preferred, because a cached build is the difference between
	// a restore and a download.
	Builds []string
	// Exclude names hosts already tried and refused, so a retry does not offer
	// the same host again.
	Exclude []string
}

// need is the memory this request asks for.
func (r PlacementRequest) need() int { return r.MemMiB }

// affinityBonus is how much a fully cached host outranks an equal one.
//
// Small on purpose. It must be able to break a near-tie and must NOT be able
// to move a machine onto a host with meaningfully less headroom: otherwise
// every machine would pile onto whichever host happened to build things, which
// is bin-packing arrived at by accident.
const affinityBonus = 0.25

// placementTieTolerance is how close two scores have to be to count as tied.
//
// # Why this is not exact equality, which is what it was
//
// Because exact equality between two floats derived from live memory readings
// essentially never happens, so the hash spread below it essentially never
// ran. freeMemMiB reports MemAvailable/1024 (cmd/hostd/fleet.go), which moves
// by megabytes between heartbeats on any host doing work. Two hosts that are
// interchangeable for placement purposes therefore scored differently every
// single time, the tie branch was skipped, and the fleet degenerated to
// "whichever host is momentarily emptiest takes everything" -- the bin-packing
// this file's header says it is avoiding.
//
// It survived because the test for it used byte-identical hosts, where the
// floats are equal by construction. Identical inputs are exactly the case a
// tolerance is not needed for, so the test could not see the bug.
//
// # The number
//
// Scores are headroom-after-placement in MiB divided by CPUCount*1024, so this
// is 64 MiB of headroom on a 4-CPU host: below any difference that should
// decide where a machine goes, and comfortably above heartbeat noise.
//
// Sixteen times smaller than affinityBonus, deliberately. A cached build must
// still outrank a host that is merely a little emptier, and would stop doing
// so if the two were close in size.
const placementTieTolerance = 1.0 / 64.0

// RankHosts orders the live fleet for one create, best first.
//
// A pure function of rows the caller already holds: no I/O, no clock beyond
// the `now` it is given, no store. That is what makes it testable and what
// keeps it off the request path's list of things that can fail.
//
// Returns an empty slice when no host can hold the machine, which the caller
// answers as "no capacity" rather than by picking someone anyway.
func RankHosts(req PlacementRequest, now time.Time, live []Host,
	caps map[string]HostCapacity, cached map[string][]string,
	vendors map[string]string, deadAfter time.Duration) []string {

	excluded := map[string]bool{}
	for _, id := range req.Exclude {
		excluded[id] = true
	}

	type scored struct {
		id    string
		score float64
		// tied carries the host row so an exact tie can fall back to the hash,
		// which needs the same candidate set on every host.
		host Host
	}
	var in []scored
	var tiedHosts []Host

	for _, h := range live {
		if excluded[h.ID] {
			continue
		}
		// A vendor-locked image only restores in its own pool. A host with no
		// vendor recorded is not in any pool, so it is skipped for a restore
		// rather than gambled on: a foreign image fails at snapshot load, deep
		// inside Firecracker, naming nothing about vendors.
		if req.Vendor != "" && vendors[h.ID] != req.Vendor {
			continue
		}
		c, ok := caps[h.ID]
		if !ok {
			// No capacity row. Not a candidate to RANK, because there is
			// nothing to rank it on, but still a candidate: a host that has
			// just joined has no row yet, and refusing to place on it would
			// make a fresh fleet unable to place anything.
			tiedHosts = append(tiedHosts, h)
			continue
		}
		if c.Draining {
			// An operator is moving machines OFF this host. Placing one on it
			// would make the drain chase its own tail.
			continue
		}
		// A capacity row older than the liveness window describes a host that
		// has stopped reporting, whatever its hosts row says. Placing on its
		// last known figures is placing on a guess.
		if deadAfter > 0 && now.Sub(time.Unix(c.UpdatedAt, 0)) > 2*deadAfter {
			continue
		}
		if c.Headroom() < req.need() {
			continue
		}

		// Headroom after placement, normalised by the host's size so a big
		// host and a small one are compared fairly rather than by raw MiB.
		denom := float64(c.CPUCount) * 1024
		if denom <= 0 {
			denom = 1024
		}
		score := float64(c.Headroom()-req.need()) / denom
		if hasAllBuilds(cached[h.ID], req.Builds) {
			score += affinityBonus
		}
		in = append(in, scored{id: h.ID, score: score, host: h})
	}

	// Sort by score, then by id, so the order is total and identical on every
	// host. Sorting by score alone would leave equal scores in whatever order
	// the live slice arrived in, and two hosts would then disagree.
	sort.Slice(in, func(i, j int) bool {
		if in[i].score != in[j].score {
			return in[i].score > in[j].score
		}
		return in[i].id < in[j].id
	})

	out := make([]string, 0, len(in)+len(tiedHosts))

	// A tie at the top is broken by the hash, so a fleet of comparable idle
	// hosts spreads rather than sending every create to whichever one is
	// momentarily emptiest.
	//
	// Within a tolerance rather than at equality: see placementTieTolerance.
	// The set is still a pure function of the scores, so every host in the
	// fleet collects the same one and ranks the create identically.
	if len(in) > 1 && in[0].score-in[1].score <= placementTieTolerance {
		var tied []Host
		for _, s := range in {
			if in[0].score-s.score > placementTieTolerance {
				break
			}
			tied = append(tied, s.host)
		}
		if winner, ok := OwnerFor(req.Name, tied); ok {
			out = append(out, winner)
			// Everyone else in score order, so a refused create still falls
			// back to the emptiest host rather than to an arbitrary one.
			for _, s := range in {
				if s.id != winner {
					out = append(out, s.id)
				}
			}
			return appendUnranked(out, tiedHosts)
		}
	}

	for _, s := range in {
		out = append(out, s.id)
	}
	// Hosts with no capacity row go last: they may well be able to hold the
	// machine, and their own admission will say, but a host that has reported
	// nothing is never preferred over one that has.
	return appendUnranked(out, tiedHosts)
}

// appendUnranked adds the hosts that could not be scored, in id order.
func appendUnranked(out []string, hosts []Host) []string {
	if len(hosts) == 0 {
		return out
	}
	ids := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ids = append(ids, h.ID)
	}
	sort.Strings(ids)
	return append(out, ids...)
}

// hasAllBuilds reports whether every build the create needs is already on the
// host. All of them: having half is a download either way.
func hasAllBuilds(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	set := make(map[string]bool, len(have))
	for _, id := range have {
		set[id] = true
	}
	for _, id := range want {
		if id == "" {
			continue
		}
		if !set[id] {
			return false
		}
	}
	return true
}
