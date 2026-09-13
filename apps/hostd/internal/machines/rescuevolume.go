package machines

import (
	"context"
	"fmt"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Rescuing a machine whose state is on a volume rather than in a snapshot.
//
// # Why this needed its own path at all
//
// Self-heal gave up on any machine with no memory image, on the reasoning that
// nothing in object storage could bring it back. That reasoning is right for a
// sandbox and exactly wrong for a volume machine: a volume machine has no
// memory image and never will, because it takes a release by BOOTING its
// rootfs -- a memory image carries the volume drive in its device state and
// could only ever be restored onto the same drive on the same host.
//
// So the machines whose data most obviously survived their host were the ones
// being written off. Its data is on a volume, in object storage, which is the
// entire point of a volume.
//
// # What makes it safe
//
// Nothing here weakens a claim. The machine row is claimed with a dead-owner
// claim, which the store re-verifies against the old owner's heartbeat at the
// moment of the write. The VOLUME is claimed the same way, and that second
// claim is the one that matters most: two hosts mounting one volume both mount
// its metadata database and destroy it.
//
// What differs from a restore is only what happens after the claim: a boot from
// the image the machine already names, rather than a restore of a snapshot that
// was never taken.

// RescueOnVolume brings a volume-backed machine up on this host after its owner
// stopped answering.
//
// Called by the self-heal loop for a machine that has a volume and an image and
// no memory build. It owns its claim, for the same reason Rescue does: the
// claim and the start happen under one per-machine lock, or something else
// decides the machine is free in between.
func (m *Manager) RescueOnVolume(ctx context.Context, row state.Machine) error {
	if row.VolumeID == "" || row.ImageRef == "" {
		return fmt.Errorf("machines: %s is not a volume-backed machine with an image", row.ID)
	}
	if m.opts.Volumes == nil {
		return ErrNoVolumes
	}

	lock := m.lockFor(row.ID)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := m.get(row.ID); ok {
		return nil // already running here
	}

	// The MACHINE first. Claiming the volume for a machine this host has not
	// been allowed to take would move a volume on the strength of a decision
	// that had not been made yet, and the machine claim is the one the store
	// re-verifies the old owner's liveness against.
	if err := m.opts.Store.ClaimMachine(ctx, row.ID, m.opts.HostID, StateCreating,
		state.WithDeadOwnerClaim(row.HostID)); err != nil {
		return fmt.Errorf("machines: claim %s: %w", row.ID, err)
	}

	fresh, err := m.opts.Store.GetMachine(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("machines: re-read %s after claiming it: %w", row.ID, err)
	}
	// The slot index on the row is the DEAD host's. Cleared for the same reason
	// a restore clears it: this host now owns the row, so a foreign index would
	// be read as one of ours, and .internal would point peers at whatever local
	// machine really holds it.
	stampSlot(fresh, nil)
	fresh.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, fresh); err != nil {
		return fmt.Errorf("machines: clear %s's old slot before rescuing it: %w", row.ID, err)
	}

	// Then the volume, which re-verifies the volume owner's liveness in its own
	// right: a volume can be owned by a host the machine row does not name.
	if _, err := m.claimVolume(ctx, fresh.VolumeID, fresh.ID); err != nil {
		m.markRescueFailed(ctx, fresh)
		return fmt.Errorf("machines: claim volume %s for %s: %w", fresh.VolumeID, fresh.ID, err)
	}

	// The machine's own token, which a boot delivers with the environment.
	// A rescue-boot IS a first boot: the old process went with its host, and
	// the new one has to be handed its environment exactly as a create does.
	fcm, err := m.bootMachine(ctx, fresh, m.token(fresh.ID), fresh.VolumeID, fresh.ImageRef, "")
	if err != nil {
		// The volume is released, or it would stay attached to a machine that
		// is not running and no other host could take it.
		_ = m.releaseVolume(ctx, fresh.VolumeID)
		m.markRescueFailed(ctx, fresh)
		return fmt.Errorf("machines: boot %s on its volume here: %w", row.ID, err)
	}
	m.put(row.ID, fcm)

	fresh.State = StateRunning
	stampSlot(fresh, fcm)
	fresh.LastActivity = time.Now().Unix()
	fresh.UpdatedAt = fresh.LastActivity
	if err := m.opts.Store.PutMachine(ctx, fresh); err != nil {
		return fmt.Errorf("machines: record %s as running here: %w", row.ID, err)
	}
	// A cold boot, recorded as one, so a client can tell this machine came back
	// without the processes it was running rather than discovering it from
	// behaviour. The dead owner's claim carries, because the CPU row belongs to
	// the same machine and the same host just took it.
	m.recordStart(ctx, fresh, state.StartColdBoot, state.WithDeadOwnerClaim(row.HostID))
	return nil
}

// markRescueFailed records that this host tried and could not, so the next tick
// re-hashes over the live set rather than finding a row stuck in creating.
func (m *Manager) markRescueFailed(ctx context.Context, row *state.Machine) {
	row.State = StateError
	stampSlot(row, nil)
	row.UpdatedAt = time.Now().Unix()
	_ = m.opts.Store.PutMachine(ctx, row)
}
