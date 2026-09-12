// Package quota bounds what one org may hold on the fleet.
//
// Counted from the LOCAL replica, joined against the tenancy table, on the
// create path only -- never on routing and never on wake. That placement is
// the whole design: a limit checked on a hot path would be a limit that makes
// every request depend on state a host might not have yet.
//
// The cap is therefore soft. Two hosts admitting a create at the same instant
// both see the same pre-create count, so a fleet can overshoot a limit by at
// most the number of hosts. That is stated rather than hidden: making it exact
// needs a read-then-write agreement across hosts, which is the coordinator
// this project is built without, and uncloud's own notes reject the pattern
// for the same reason.
package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Defaults apply to an org with no row of its own. Generous enough that a
// real workload never meets them and small enough that one stolen key cannot
// fill a host.
var Defaults = state.Quota{
	MaxMachines:  20,
	MaxVCPUs:     40,
	MaxMemMiB:    65536,
	MaxVolumeGiB: 100,
	MaxBuilds:    2,
	// Fifty gibibytes of checkpoints. An ordinary machine's checkpoints are
	// tens of megabytes each, because they diff against the template, so this
	// is hundreds of them; what it stops is the unbounded case, an agent
	// checkpointing after every message on a machine nobody destroys.
	MaxSnapshotGiB: 50,
}

// Delta is what a request is about to add.
type Delta struct {
	Machines  int
	VCPUs     int
	MemMiB    int
	VolumeGiB int
	// SnapshotGiB is what a checkpoint is about to add. In practice 1, since
	// what a checkpoint will weigh is not known until it has been taken: the
	// limit admits while the org is under the line and overshoots by at most
	// one checkpoint, which is this package's soft-cap semantics.
	SnapshotGiB int
}

// Exceeded names the limit that refused a request, so the client is told what
// to raise rather than only that something was too big.
type Exceeded struct {
	Quota string // machines|vcpus|mem_mib|volume_gib|builds
	Limit int
	Used  int
	// Scope is "host" for a limit that is per host rather than fleet-wide,
	// which is true of builds alone: a build is not a replicated object, so
	// there is nothing fleet-wide to count.
	Scope string
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("quota exceeded: %s (limit %d, used %d)", e.Quota, e.Limit, e.Used)
}

// For returns an org's limits, or the defaults when it has no row.
//
// A missing row is not zero: zero would freeze every new org the moment it
// appeared. Freezing an org is done by WRITING a row of zeroes.
func For(ctx context.Context, st state.Store, orgID string) state.Quota {
	q, err := limitsFor(ctx, st, orgID)
	if err != nil {
		out := Defaults
		out.OrgID = orgID
		return out
	}
	return q
}

// limitsFor is For with the error kept.
//
// The distinction matters on the create path: a store that is briefly
// unreachable must not read as "this org has no row", because the defaults
// are generous and an org deliberately frozen with a row of zeroes would be
// admitted by the blip. A caller that has nowhere to put an error uses For.
func limitsFor(ctx context.Context, st state.Store, orgID string) (state.Quota, error) {
	q, err := st.GetQuota(ctx, orgID)
	if errors.Is(err, state.ErrNotFound) || (err == nil && q == nil) {
		out := Defaults
		out.OrgID = orgID
		return out, nil
	}
	if err != nil {
		return state.Quota{}, fmt.Errorf("quota: get quota %q: %w", orgID, err)
	}
	out := *q
	// A row written before org_snapshot_quotas existed carries zero here, and
	// zero would refuse every checkpoint. An unset limit is the default, the
	// same reading an org with no quota row at all gets.
	if out.MaxSnapshotGiB == 0 {
		out.MaxSnapshotGiB = Defaults.MaxSnapshotGiB
	}
	return out, nil
}

// Check counts what an org already holds and refuses a delta that would take
// it past its limits.
//
// Three list reads of local state, once per create. Deliberately not a cached
// counter: a second copy of a number that has to stay right is a second thing
// to keep right, and the create path already lists machines to check the name
// is free.
// BuilderNamePrefix is the name prefix of a hostd-owned builder machine, the
// per-org guest a build's BuildKit daemon runs in.
//
// It lives here rather than in the machines package because quota must not
// depend on machines, which depends on quota. machines.BuilderName is built
// from this constant, so the two cannot drift.
const BuilderNamePrefix = "builder-"

func Check(ctx context.Context, st state.Store, orgID string, d Delta) error {
	if orgID == "" {
		// A create with no org can only come from an unauthenticated internal
		// path. There is nothing to count against and nothing to protect.
		return nil
	}
	limits, err := limitsFor(ctx, st, orgID)
	if err != nil {
		return err
	}

	owned, err := ownedIDs(ctx, st, orgID)
	if err != nil {
		return err
	}

	machines, err := st.ListMachines(ctx)
	if err != nil {
		return fmt.Errorf("quota: list machines: %w", err)
	}
	var usedMachines, usedVCPUs, usedMem int
	for _, m := range machines {
		// A destroyed machine holds nothing. Its row lingers until the reaper
		// collects it, and counting tombstones would make an org's limit fall
		// over its lifetime rather than over what it is running.
		if _, mine := owned[m.ID]; m.State == state.StateDestroyed || !mine {
			continue
		}
		// A builder is not a workload the org asked for. hostd creates one per
		// org per host to run that org's Dockerfile inside a microVM instead of
		// on the host, and destroys it on its own schedule. Counting it would
		// make a deploy fail against a limit the org never spent, and an org
		// sitting exactly on its machine limit could never build again.
		if strings.HasPrefix(m.Name, BuilderNamePrefix) {
			continue
		}
		usedMachines++
		usedVCPUs += m.VCPUs
		usedMem += m.MemMiB
	}

	volumes, err := st.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("quota: list volumes: %w", err)
	}
	usedVolume := 0
	for _, v := range volumes {
		if _, mine := owned[v.ID]; mine {
			usedVolume += v.SizeMiB / 1024
		}
	}

	// What this org's checkpoints hold. Counted from the rows the same way
	// everything else here is, rather than from a running total: a counter
	// would drift the moment an upload failed or a retention pass deleted
	// something, and nothing would ever correct it.
	usedSnapshot := 0
	if d.SnapshotGiB > 0 {
		usedSnapshot = snapshotGiBOf(ctx, st, owned)
	}

	for _, c := range []struct {
		name             string
		used, add, limit int
	}{
		{"machines", usedMachines, d.Machines, limits.MaxMachines},
		{"vcpus", usedVCPUs, d.VCPUs, limits.MaxVCPUs},
		{"mem_mib", usedMem, d.MemMiB, limits.MaxMemMiB},
		{"volume_gib", usedVolume, d.VolumeGiB, limits.MaxVolumeGiB},
		{"snapshot_gib", usedSnapshot, d.SnapshotGiB, limits.MaxSnapshotGiB},
	} {
		if c.add > 0 && c.used+c.add > c.limit {
			return &Exceeded{Quota: c.name, Limit: c.limit, Used: c.used}
		}
	}
	return nil
}

// ownedIDs is the set of object ids an org owns, from the tenancy table.
func ownedIDs(ctx context.Context, st state.Store, orgID string) (map[string]struct{}, error) {
	rows, err := st.ListTenancy(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota: list tenancy: %w", err)
	}
	out := make(map[string]struct{}, len(rows))
	for _, t := range rows {
		if t.OrgID == orgID {
			out[t.ID] = struct{}{}
		}
	}
	return out, nil
}

// HostGate bounds concurrent builds for one org ON THIS HOST.
//
// Per host, and the refusal says so, because a build is not a replicated
// object: there is no row for one, so there is nothing fleet-wide to count.
// The honest limit is the one this host can enforce from what it knows.
type HostGate struct {
	mu sync.Mutex
	n  map[string]int
}

// Acquire takes a slot for an org, reporting how many are in use and whether
// the caller got one. Every successful Acquire needs a matching Release.
func (g *HostGate) Acquire(orgID string, limit int) (used int, ok bool) {
	if g == nil {
		return 0, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == nil {
		g.n = map[string]int{}
	}
	if g.n[orgID] >= limit {
		return g.n[orgID], false
	}
	g.n[orgID]++
	return g.n[orgID], true
}

// Release gives a slot back. Safe on a nil gate and on an org that holds none,
// so a deferred Release never has to be conditional.
func (g *HostGate) Release(orgID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n[orgID] > 0 {
		g.n[orgID]--
	}
}
