package machines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

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

// startNewMachine brings up a machine that has just been created.
//
// Two paths, and the split is forced rather than chosen. A machine with a
// volume has to boot, because a drive cannot be added to a snapshot being
// restored; a machine with its own image has to boot, because the golden
// template's memory describes the golden template's disk and resuming it
// against another root filesystem is a guest whose memory and disk have never
// met. Everything else restores, which is what makes a create sub-second.
func (m *Manager) startNewMachine(ctx context.Context, row *state.Machine,
	token, volumeID, image, appCmd string) (*fc.Machine, error) {

	if volumeID != "" || image != "" {
		return m.bootMachine(ctx, row, token, volumeID, image, appCmd)
	}
	return m.createFromTemplate(ctx, row, token, appCmd)
}

// startForRelease is the rollout's entry point: restore a machine from a
// release's build pair rather than boot it from the release's image.
func (m *Manager) startForRelease(ctx context.Context, row *state.Machine,
	token, memBuildID, rootfsBuildID string) (*fc.Machine, error) {

	return m.createFromRelease(ctx, row, token, memBuildID, rootfsBuildID)
}

// createFromTemplate restores a brand-new machine from the golden template.
//
// A create is a restore. The alternative -- booting a kernel -- takes twenty
// seconds and produces a machine indistinguishable from this one.
func (m *Manager) createFromTemplate(ctx context.Context, row *state.Machine,
	token, appCmd string) (*fc.Machine, error) {
	t, err := m.EnsureTemplate(ctx)
	if err != nil {
		return nil, err
	}

	// Pin it. Every image this machine ever writes is a diff against this
	// template, and the ranges it does not change resolve against the parent
	// by offset -- so restoring it against a DIFFERENT template returns a
	// guest stitched from two machines. Which template a host holds is not
	// fleet-wide: it changes when the golden template is rebuilt, and a host
	// whose cache was cleared mints its own. Recording it here is what lets
	// any host restore this machine correctly, forever.
	row.TemplateMemBuildID = t.MemBuildID.String()
	row.TemplateRootfsBuildID = t.RootfsBuildID.String()

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
		// The restore above already bound the responder inside this
		// namespace, and Kill takes the namespace with it. Unbound here, the
		// socket keeps the dead namespace alive and its goroutines run for
		// the life of the process.
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}

	// The ONE call site. See the note at the top of env.go: the wake path
	// resumes a snapshot in which the application is already running, and has
	// no business delivering an environment to it.
	if err := m.deliverEnv(ctx, row, slot, appCmd); err != nil {
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("deliver env: %w", err)
	}
	return fcm, nil
}

// wakeFromSuspend restores a machine from its own last suspend.
func (m *Manager) wakeFromSuspend(ctx context.Context, row *state.Machine) (*fc.Machine, error) {
	t, err := m.templateFor(ctx, row)
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

// takeSlot hands a bring-up the netns slot it will run in.
//
// A row that names an index on THIS host is a machine that suspended here with
// its slot kept: Suspend holds a service replica's index so the replica stays
// resolvable while it sleeps (see the comment there), and this is where that
// reservation is consumed. Taking the same index back is what stops the pool
// growing by one per wake, and it keeps the replica's mesh address still, so a
// peer that resolved the name before the suspend is still right after it. Only
// a rescue moves the address, which is why .internal answers keep their
// near-zero TTL.
//
// Everything else takes a fresh index: a create, whose row names no slot yet;
// a rescue, whose index belongs to the dead host's pool and is cleared before
// it gets here; and a reservation the pool will not honour, which is logged
// and then treated as absent, because a wake that fails over bookkeeping is
// worse than a wake onto a different index.
func (m *Manager) takeSlot(row *state.Machine) (*netns.Slot, error) {
	if row.Slot > 0 && row.HostID == m.opts.HostID {
		slot, err := m.pool.Reserve(row.Slot, row.ID)
		if err == nil {
			return slot, nil
		}
		slog.Warn("a machine's kept slot could not be reused; it comes up in a "+
			"fresh one and its mesh address moves",
			"machine", row.ID, "slot", row.Slot, "err", err)
	}
	return m.pool.Take(row.ID)
}

func (m *Manager) restore(ctx context.Context, row *state.Machine, backends fc.Backends,
	snapKey, localDir string, immutable bool) (*fc.Machine, *netns.Slot, error) {

	// A volume has to be mounted on THIS host before anything can open its
	// image, and the claim is what stops two hosts mounting it at once. Every
	// local bring-up funnels through here -- create, wake, rescue and
	// checkpoint restore alike -- so there is one place this can be missed
	// rather than four.
	if row.VolumeID != "" {
		if _, err := m.claimVolume(ctx, row.VolumeID, row.ID); err != nil {
			return nil, nil, fmt.Errorf("machines: attach volume %s for %s: %w",
				row.VolumeID, row.ID, err)
		}
	}

	slot, err := m.takeSlot(row)
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

	// Every restore lands here -- create, wake, rescue, checkpoint rollback --
	// so this is the one place a namespace becomes servable, and the one place
	// the responder has to follow it to.
	m.bindDiscovery(row.ID, slot)

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

// Rescue takes a machine from a host that has stopped responding and brings it
// up here.
//
// Everything host-local is minted fresh: this host may never have seen the
// machine, so there is no slot reservation, no netns, no local disk and no
// breadcrumb to reuse. What survives is the row, and the row is enough --
// which is only true because nothing host-specific ever enters a snapshot. A
// guest sees the same addresses on every host, and all the slot-specific
// routing lives in the namespace, rebuilt from scratch here.
//
// The URL does not change, because routing follows the row. The agent token
// does not change, because its hash is on the row rather than on the old host.
func (m *Manager) Rescue(ctx context.Context, row state.Machine) error {
	lock := m.lockFor(row.ID)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := m.get(row.ID); ok {
		return nil // already running here
	}

	// The claim re-checks the old owner's liveness inside the store: the gap
	// between deciding to rescue and writing is exactly where a host returns.
	if err := m.opts.Store.ClaimMachine(ctx, row.ID, m.opts.HostID, StateCreating,
		state.WithDeadOwnerClaim(row.HostID)); err != nil {
		return fmt.Errorf("machines: claim %s: %w", row.ID, err)
	}

	fresh, err := m.opts.Store.GetMachine(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("machines: re-read %s after claiming it: %w", row.ID, err)
	}
	// The index on the row is the DEAD host's, and the claim above has already
	// made this host the owner -- so takeSlot's owner check, which is what
	// keeps a foreign index out of this pool, would now read it as one of
	// ours. Cleared here so a rescue takes a fresh index, which is the only
	// kind it can have.
	stampSlot(fresh, nil)

	fcm, err := m.wakeFromSuspend(ctx, fresh)
	if err != nil {
		fresh.State = StateError
		stampSlot(fresh, nil)
		fresh.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, fresh)
		return fmt.Errorf("machines: restore %s here: %w", row.ID, err)
	}
	m.put(row.ID, fcm)

	fresh.State = StateRunning
	// A rescued machine lands in a slot this host chose, so its mesh address
	// changes. That is exactly why .internal answers carry a near-zero TTL.
	stampSlot(fresh, fcm)
	fresh.LastActivity = time.Now().Unix()
	fresh.UpdatedAt = fresh.LastActivity
	if err := m.opts.Store.PutMachine(ctx, fresh); err != nil {
		return err
	}
	// This host bills it from here. The dead owner's ledger ended at its last
	// tick, so the seam between the two is bounded by one tick and neither
	// side double-bills.
	org := ""
	if t, terr := m.opts.Store.GetTenancy(ctx, fresh.ID); terr == nil && t != nil {
		org = t.OrgID
	}
	m.opts.Usage.Open(fresh.ID, org, StateRunning, fresh.VCPUs, fresh.MemMiB,
		m.volumeGiB(ctx, fresh.VolumeID))
	return nil
}

// StopLocal tears down a machine this host is running but no longer owns.
//
// Called by the self-heal loop when the row names another host. It stops the
// processes and releases the slot WITHOUT touching the row: the row already
// belongs to someone else, and writing to it would be the single-writer
// violation this is cleaning up after.
func (m *Manager) StopLocal(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	fcm, ok := m.get(id)
	if !ok {
		return nil
	}
	slotIdx := 0
	if fcm.Slot != nil {
		slotIdx = fcm.Slot.Idx
	}
	m.releaseDiscovery(id)

	err := fcm.Kill()
	m.drop(id)
	if slotIdx > 0 {
		m.pool.Return(slotIdx)
	}
	// And stop metering it here: the row names another host, so the machine is
	// billed by its rescuer now. Two hosts each with an open interval for the
	// same machine is the one way this ledger can double-bill.
	m.opts.Usage.Close(id)

	// And let go of its volume's filesystem, which killing Firecracker does
	// not do. This host was partitioned and the machine has been rescued
	// elsewhere, so the rescuer has already restored that volume's metadata
	// and mounted it -- and a second juicefs mount here, still holding the
	// database this host had, writes into the same object-storage prefix
	// behind the new owner's back. Nothing about the row: it belongs to
	// someone else now, and writing to it is the violation this is cleaning
	// up after.
	if row, rerr := m.opts.Store.GetMachine(ctx, id); rerr == nil &&
		row.VolumeID != "" && m.opts.Volumes != nil {
		if verr := m.opts.Volumes.Detach(ctx, row.VolumeID); verr != nil {
			err = errors.Join(err, fmt.Errorf("release volume %s: %w", row.VolumeID, verr))
		}
	}
	return err
}

// RunningIDs lists the machines this host currently has processes for.
func (m *Manager) RunningIDs() []string { return m.Running() }
