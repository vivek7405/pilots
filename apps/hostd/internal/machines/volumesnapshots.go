package machines

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
	"github.com/vivek7405/pilots/hostd/internal/volumes"
)

// Point-in-time copies of a volume, as the lifecycle layer sees them.
//
// The volumes package knows how to clone and restore. What it cannot know is
// the MACHINE's state, and that is the whole of what this file adds: a
// snapshot taken under a live guest captures a half-written filesystem, and a
// restore under a live guest replaces the disk out from under it.

// SnapshotVolume takes a point-in-time copy of a volume.
//
// The machine holding it is quiesced first, and how depends on what it is
// doing. Running: the guest is PAUSED for the clone, milliseconds, because a
// clone taken while the guest is writing captures a filesystem mid-update --
// which mounts, and then fails somewhere later, on a fork nobody will connect
// back to this moment. Suspended or unattached: nothing is writing, so the
// clone is taken as it stands.
func (m *Manager) SnapshotVolume(ctx context.Context, volumeID string) (string, error) {
	if m.opts.Volumes == nil {
		return "", ErrNoVolumes
	}
	v, err := m.opts.Store.GetVolume(ctx, volumeID)
	if err != nil {
		return "", err
	}
	if v.HostID != "" && v.HostID != m.opts.HostID {
		return "", fmt.Errorf("machines: volume %s is mounted on %s, not here: %w",
			volumeID, v.HostID, state.ErrNotOwner)
	}
	stamp := volumes.SnapshotStamp(time.Now())

	// A volume with no machine, or one whose machine is not running, is
	// already still. Nothing to pause.
	if v.MachineID == "" {
		return stamp, m.opts.Volumes.Snapshot(ctx, volumeID, stamp)
	}
	row, err := m.opts.Store.GetMachine(ctx, v.MachineID)
	if err != nil || row.State != StateRunning {
		return stamp, m.opts.Volumes.Snapshot(ctx, volumeID, stamp)
	}

	lock := m.lockFor(row.ID)
	lock.Lock()
	defer lock.Unlock()

	fcm, ok := m.get(row.ID)
	if !ok {
		// The row says running and no process is here. Whatever that is, it is
		// not a machine writing to this volume.
		return stamp, m.opts.Volumes.Snapshot(ctx, volumeID, stamp)
	}
	// Paused for the clone and resumed straight after. The guest sees a few
	// milliseconds of stopped clock, which is the same cost a checkpoint's
	// freeze pays and for the same reason: a consistent image is worth more
	// than the milliseconds.
	if err := fcm.WhilePaused(ctx, func() error {
		return m.opts.Volumes.Snapshot(ctx, volumeID, stamp)
	}); err != nil {
		return "", fmt.Errorf("machines: snapshot volume %s: %w", volumeID, err)
	}
	slog.Info("snapshotted a volume while its guest was paused",
		"volume", volumeID, "machine", row.ID, "snapshot", stamp)
	return stamp, nil
}

// ListVolumeSnapshots names a volume's snapshots, newest first.
func (m *Manager) ListVolumeSnapshots(ctx context.Context, volumeID string) ([]string, error) {
	if m.opts.Volumes == nil {
		return nil, ErrNoVolumes
	}
	return m.opts.Volumes.ListSnapshots(volumeID)
}

// DeleteVolumeSnapshot removes one snapshot.
//
// Nothing about the machine matters here, which is why there is no state
// check: deleting a snapshot touches only the clone, never the live image, so
// a running guest is unaffected. What it frees is the blocks only that
// snapshot still held.
func (m *Manager) DeleteVolumeSnapshot(ctx context.Context, volumeID, stamp string) error {
	if m.opts.Volumes == nil {
		return ErrNoVolumes
	}
	v, err := m.opts.Store.GetVolume(ctx, volumeID)
	if err != nil {
		return err
	}
	if v.HostID != "" && v.HostID != m.opts.HostID {
		return fmt.Errorf("machines: volume %s is mounted on %s, not here: %w",
			volumeID, v.HostID, state.ErrNotOwner)
	}
	return m.opts.Volumes.DeleteSnapshot(ctx, volumeID, stamp)
}

// ForkVolumeSnapshot makes a NEW volume holding one snapshot's contents.
//
// The operation a recovery is built on. Restoring IN PLACE replaces the data
// you are trying to compare against; forking gives you both, which is what
// somebody looking at a bad migration actually needs.
//
// This copies BYTES, and says so: JuiceFS slice ids are per filesystem, so a
// new volume shares nothing with the old one. It is the one operation in this
// file that is not metadata, and it is proportional to what was written.
func (m *Manager) ForkVolumeSnapshot(ctx context.Context, volumeID, stamp, name string) (*state.Volume, error) {
	if m.opts.Volumes == nil {
		return nil, ErrNoVolumes
	}
	src, err := m.opts.Store.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if src.HostID != "" && src.HostID != m.opts.HostID {
		return nil, fmt.Errorf("machines: volume %s is mounted on %s, not here: %w",
			volumeID, src.HostID, state.ErrNotOwner)
	}
	// The snapshot has to exist before a volume is made for it, or a failure
	// leaves an empty volume nobody asked for and nothing will clean up.
	stamps, err := m.opts.Volumes.ListSnapshots(volumeID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, s := range stamps {
		if s == stamp {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("machines: %s has no snapshot %s: %w", volumeID, stamp, ErrNotFound)
	}

	if name == "" {
		name = src.Name + "-" + stamp
	}
	fork, err := m.CreateVolume(ctx, api.CreateVolumeRequest{
		Name: name, SizeGiB: src.SizeMiB / 1024, MountPath: src.MountPath,
	})
	if err != nil {
		return nil, err
	}
	if err := m.opts.Volumes.CopySnapshotTo(ctx, volumeID, stamp, m.opts.Volumes.ImagePath(fork.ID)); err != nil {
		// The volume exists and is empty, which is worse than not existing: it
		// would be mounted, found blank, and quietly used. Destroyed here so a
		// failed fork leaves nothing behind.
		_ = m.releaseVolume(ctx, fork.ID)
		return nil, fmt.Errorf("machines: fill %s from %s@%s: %w", fork.ID, volumeID, stamp, err)
	}
	slog.Info("forked a volume from a snapshot",
		"from", volumeID, "snapshot", stamp, "to", fork.ID)
	return fork, nil
}

// RestoreVolumeSnapshot puts a snapshot back as the volume's live image.
//
// A RUNNING machine is refused. Replacing the disk under a live guest is not a
// restore, it is corruption with a nicer name: the guest's cached filesystem
// metadata describes the image that was there a moment ago.
//
// A SUSPENDED machine is allowed, and loses its memory image. That is not
// incidental -- a memory image holds cached ext4 state from the OLD disk, and
// waking it onto a restored one corrupts the filesystem within seconds. The
// machine cold-boots instead, which is slower and correct.
func (m *Manager) RestoreVolumeSnapshot(ctx context.Context, volumeID, stamp string) error {
	if m.opts.Volumes == nil {
		return ErrNoVolumes
	}
	v, err := m.opts.Store.GetVolume(ctx, volumeID)
	if err != nil {
		return err
	}
	if v.HostID != "" && v.HostID != m.opts.HostID {
		return fmt.Errorf("machines: volume %s is mounted on %s, not here: %w",
			volumeID, v.HostID, state.ErrNotOwner)
	}

	var row *state.Machine
	if v.MachineID != "" {
		if row, err = m.opts.Store.GetMachine(ctx, v.MachineID); err != nil {
			return err
		}
		if row.State == StateRunning {
			return fmt.Errorf("%w: %s is running on volume %s; suspend or destroy "+
				"it before restoring a snapshot, because replacing the disk under a "+
				"live guest corrupts it", api.ErrConflict, row.ID, volumeID)
		}
		lock := m.lockFor(row.ID)
		lock.Lock()
		defer lock.Unlock()
	}

	if err := m.opts.Volumes.RestoreSnapshot(ctx, volumeID, stamp); err != nil {
		return err
	}

	if row != nil && row.MemBuildID != "" {
		// The memory image cached the OLD filesystem. Dropping it makes the
		// next wake a cold boot, which reads the restored disk honestly.
		superseded := row.MemBuildID
		row.MemBuildID = ""
		row.UpdatedAt = time.Now().Unix()
		if err := m.opts.Store.PutMachine(ctx, row); err != nil {
			return fmt.Errorf("machines: drop %s's memory image after a volume restore: %w",
				row.ID, err)
		}
		slog.Info("dropped a machine's memory image after restoring its volume; "+
			"it will cold-boot", "machine", row.ID, "volume", volumeID, "snapshot", stamp)
		m.discardBuilds(ctx, superseded)
	}
	return nil
}
