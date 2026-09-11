package machines

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// builderVCPUs and builderMemMiB size a builder machine.
//
// fly's shared builder is 4 CPU / 4 GB and its per-app builders are the same
// shape, which is the closest published number for this workload. It also
// matches what the host daemon was already allowed to take when it ran here:
// CPUQuota=400% and MemoryMax=8G on its user slice. Four vCPUs and 4 GiB is
// that budget, now charged to a machine that can be suspended rather than to
// a daemon that cannot.
const (
	builderVCPUs  = 4
	builderMemMiB = 4096
)

// builderIdleTimeout is how long a builder stays awake after its last build.
//
// Longer than the 60 s a sandbox gets, and deliberately. Suspending a builder
// writes a memory image of its whole working set, and a developer iterating on
// a Dockerfile pays that write plus the restore on every edit. Five minutes
// covers an edit-build-edit loop without keeping the machine up between
// sessions.
const builderIdleTimeout = 300

// builderKnobs is a PARTIAL policy: DecodeKnobs merges it onto the defaults,
// so the builder keeps suspend-on-idle and differs only in how long it waits.
var builderKnobs = json.RawMessage(
	fmt.Sprintf(`{"idle_timeout":%d}`, builderIdleTimeout))

// EnsureBuilder creates or wakes the builder machine for one org ON THIS HOST
// and returns the address buildctl should dial.
//
// Local only, and that is the design rather than an optimisation. It reads
// this host's own rows, creates on this host, and waits for nothing else: no
// lookup of where an org's builder might be, no forward to the host that has
// one, no pool to be scheduled into. A build is served entirely by the host
// that received it, which is what keeps POST /v1/builds free of a dependency
// on any particular machine being alive.
//
// The returned release must be called when the build ends. It drops the
// in-flight count that stops the idle monitor suspending the daemon mid-solve.
func (m *Manager) EnsureBuilder(ctx context.Context, orgID string) (string, func(), error) {
	name := BuilderName(orgID, m.opts.HostID)

	id, err := m.findBuilder(ctx, name)
	if err != nil {
		return "", nil, err
	}
	if id == "" {
		if id, err = m.createBuilder(ctx, orgID, name); err != nil {
			return "", nil, err
		}
	}

	// Running or suspended, the same call handles both: Wake returns
	// immediately for a machine that is already up, and it is safe from any
	// in-process caller.
	if _, ok := m.get(id); !ok {
		if err := m.Wake(ctx, id); err != nil {
			return "", nil, fmt.Errorf("machines: wake the builder %s: %w", id, err)
		}
	}

	slot, ok := m.SlotFor(id)
	if !ok {
		return "", nil, fmt.Errorf("machines: builder %s is not running: %w", id, ErrNotFound)
	}

	// Bracket the whole build. Without this the idle monitor can suspend the
	// daemon in the middle of a solve, which surfaces as a build that dies
	// against a connection that simply stopped answering.
	m.Begin(id)
	m.Touch(context.WithoutCancel(ctx), id)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		m.End(id)
		m.Touch(context.WithoutCancel(ctx), id)
	}
	return slot.BuildkitAddr(), release, nil
}

// findBuilder returns this host's builder row for a name, or "" if there is
// none to reuse.
//
// Matched on name AND host: the name already carries a host suffix, so a row
// from another host cannot match, but the explicit check is what makes that a
// property of this function rather than of the naming scheme. A destroyed row
// is a tombstone and is not reused.
func (m *Manager) findBuilder(ctx context.Context, name string) (string, error) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Name == name && row.HostID == m.opts.HostID && row.State != state.StateDestroyed {
			return row.ID, nil
		}
	}
	return "", nil
}

// createBuilder makes this host's builder for an org.
//
// It carries no App on purpose. The tenant filter is keyed on App, so a
// machine without one falls into the unconditional guest-to-guest drop, which
// is exactly the posture a builder wants: it needs the internet for a base
// image and a package index, and nothing on the internal network at all.
func (m *Manager) createBuilder(ctx context.Context, orgID, name string) (string, error) {
	if _, err := m.EnsureBuilderTemplate(ctx); err != nil {
		return "", err
	}

	start := time.Now()
	row, err := m.Create(ctx, api.CreateMachineRequest{
		Name:   name,
		VCPUs:  builderVCPUs,
		MemMiB: builderMemMiB,
		OrgID:  orgID,
		// Internal is what lets this create take a reserved builder- name.
		Internal: true,
		// Visible to the org that owns it, and destroyable by it, which is
		// what fly does with fly-builder-*. A machine with no tenancy row
		// would be admin-only, so the org could neither see the resource it
		// is being given nor clear a wedged one.
		Labels: map[string]string{"pilot.role": "builder"},
		Knobs:  builderKnobs,
	})
	if err != nil {
		return "", fmt.Errorf("machines: create the builder for org %q: %w", orgID, err)
	}
	slog.Info("created a builder machine", "id", row.ID, "name", name,
		"org", orgID, "seconds", int(time.Since(start).Seconds()))
	return row.ID, nil
}
