package machines

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// Creating, waking and rolling back to a checkpoint are one operation with
// different artifacts. They differ only in which builds the machine reads
// through, which is why they all funnel into restoreInstant below.

// suspendSnapKey is where a machine's suspend vmstate lives. One key per
// machine, overwritten on every suspend.
func suspendSnapKey(machineID string) string {
	return filepath.Join("machines", machineID, "suspend", fc.SnapFile)
}

// prefetchKey is where a machine's recorded fault order lives.
func prefetchKey(machineID string) string {
	return filepath.Join("machines", machineID, "suspend", "prefetch.txt")
}

// checkpointSnapKey is where a checkpoint's vmstate lives. Written once under
// its own id, so it is never overwritten by a later suspend.
func checkpointSnapKey(machineID, checkpointID string) string {
	return filepath.Join("machines", machineID, "checkpoints", checkpointID, fc.SnapFile)
}

// createFromTemplate restores a brand-new machine from the golden template.
//
// A create is a restore. The alternative -- booting a kernel -- takes twenty
// seconds and produces a machine indistinguishable from this one.
func (m *Manager) createFromTemplate(ctx context.Context, row *state.Machine, token string) (*fc.Machine, error) {
	t, err := m.EnsureTemplate(ctx)
	if err != nil {
		return nil, err
	}

	fcm, slot, err := m.restoreInstant(ctx, row, fc.Backends{
		MemBuildID:        t.MemBuildID,
		RootfsTemplateDir: m.rootfsTemplateDir(t),
		CacheRoot:         m.buildDir(),
	}, t.SnapKey)
	if err != nil {
		return nil, err
	}

	// Replace the template's placeholder credential with this machine's own.
	//
	// Fatal, not a warning. Without it hostd holds a token the guest does not,
	// falls back to the placeholder, and everything keeps working -- on a
	// credential every machine created from this template also has.
	if err := m.installToken(ctx, slot, token); err != nil {
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}
	return fcm, nil
}

// wakeFromSuspend restores a machine from its own last suspend.
func (m *Manager) wakeFromSuspend(ctx context.Context, row *state.Machine) (*fc.Machine, error) {
	t, err := m.EnsureTemplate(ctx)
	if err != nil {
		return nil, err
	}

	memBuild, err := uuid.Parse(row.MemBuildID)
	if err != nil {
		return nil, fmt.Errorf("machines: machine %s has no usable memory build (%q): %w",
			row.ID, row.MemBuildID, err)
	}

	backends := fc.Backends{
		MemBuildID: memBuild,
		// The memory image is a diff against the template, so the template has
		// to be attached or every unchanged page resolves to nothing.
		MemParentBuildID:  t.MemBuildID,
		RootfsTemplateDir: m.rootfsTemplateDir(t),
		CacheRoot:         m.buildDir(),
	}
	// An absent rootfs build is normal: the machine wrote nothing to disk and
	// reads the template directly.
	if row.RootfsBuildID != "" {
		if backends.RootfsDiffID, err = uuid.Parse(row.RootfsBuildID); err != nil {
			return nil, fmt.Errorf("machines: machine %s has an unusable disk build (%q): %w",
				row.ID, row.RootfsBuildID, err)
		}
	}

	m.fetchPrefetch(ctx, row.ID, prefetchKey(row.ID))

	fcm, _, err := m.restoreInstant(ctx, row, backends, suspendSnapKey(row.ID))
	return fcm, err
}

// fetchPrefetch pulls a machine's recorded fault order back from storage.
//
// Best effort by design. A machine waking on a host that has never run it has
// no local copy, and a machine that has never been woken has no recording at
// all; both simply wake with a cold cache, which is slower and not wrong.
func (m *Manager) fetchPrefetch(ctx context.Context, machineID, key string) {
	dest := uffd.PrefetchFor(m.stateDir(machineID))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return
	}
	if err := m.opts.Uploader.GetToFile(ctx, key, dest); err != nil {
		slog.Debug("no stored fault order; this wake will be cold",
			"machine", machineID, "err", err)
	}
}

// restoreInstant is the shared body of every restore.
func (m *Manager) restoreInstant(ctx context.Context, row *state.Machine,
	backends fc.Backends, snapKey string) (*fc.Machine, *netns.Slot, error) {

	return m.restore(ctx, row, backends, snapKey,
		filepath.Join(m.opts.CacheRoot, "snapshots", row.ID), false)
}

// restoreInstantImmutable restores from an artifact set written once under its
// own id -- a checkpoint -- so its local copy can be trusted rather than
// re-fetched.
func (m *Manager) restoreInstantImmutable(ctx context.Context, row *state.Machine,
	backends fc.Backends, snapKey, localDir string) (*fc.Machine, *netns.Slot, error) {

	return m.restore(ctx, row, backends, snapKey, localDir, true)
}

func (m *Manager) restore(ctx context.Context, row *state.Machine, backends fc.Backends,
	snapKey, localDir string, immutable bool) (*fc.Machine, *netns.Slot, error) {

	slot, err := m.pool.Take(row.ID)
	if err != nil {
		return nil, nil, err
	}
	mac, err := fc.GenerateMAC()
	if err != nil {
		m.pool.Return(slot.Idx)
		return nil, nil, err
	}

	fcm, err := fc.RestoreInstant(ctx, fc.InstantConfig{
		Config:        m.machineFCConfig(row, slot, mac),
		Backends:      backends,
		LocalDir:      localDir,
		AgentToken:    m.token(row.ID),
		SnapKey:       snapKey,
		SnapImmutable: immutable,
		Env:           m.opts.HandlerEnv,
	}, m.opts.Uploader, m.opts.BlockStore, m.opts.NBDDevices)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, nil, err
	}

	if err := fcm.Persist(); err != nil {
		// Not fatal: the machine is running and serving. But a restart will
		// not re-adopt it, which is worth shouting about.
		slog.Error("restored machine's breadcrumbs were not written",
			"machine", row.ID, "err", err)
	}
	return fcm, slot, nil
}

// snapshotOpts names the builds a snapshot is diffed against.
func (m *Manager) snapshotOpts(t *Template) fc.SnapshotOpts {
	return fc.SnapshotOpts{
		MemParentDir:      m.memParentDir(t),
		RootfsTemplateDir: m.rootfsTemplateDir(t),
		BuildDir:          m.buildDir(),
	}
}
