package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
	"github.com/vivek7405/pilots/hostd/internal/volumes"
)

// VolumeManager is the volume surface this package drives. An interface so a
// host with no object storage configured simply has none, and so the machine
// lifecycle can be tested without JuiceFS.
type VolumeManager interface {
	Create(ctx context.Context, name string, sizeMiB int, mountPath string) (*state.Volume, error)
	Attach(ctx context.Context, v *state.Volume) error
	Detach(ctx context.Context, id string) error
	ImagePath(id string) string
}

// ErrNoVolumes reports a host that cannot serve volumes at all.
var ErrNoVolumes = errors.New("machines: volumes are not configured on this host")

// CreateVolume makes a volume on this host and publishes its row.
//
// The row is written AFTER the volume exists, and that order matters more here
// than it does for a machine: a row naming a volume that was never formatted
// is a volume every later attach fails against, on every host, forever.
func (m *Manager) CreateVolume(ctx context.Context, req api.CreateVolumeRequest) (*state.Volume, error) {
	if m.opts.Volumes == nil {
		return nil, ErrNoVolumes
	}
	if req.SizeGiB <= 0 {
		return nil, fmt.Errorf("machines: a volume needs size_gib")
	}

	v, err := m.opts.Volumes.Create(ctx, req.Name, req.SizeGiB*1024, req.MountPath)
	if err != nil {
		return nil, err
	}
	if err := m.opts.Store.PutVolume(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

// ListVolumes reads the fleet's volumes from local state.
func (m *Manager) ListVolumes(ctx context.Context) ([]state.Volume, error) {
	return m.opts.Store.ListVolumes(ctx)
}

// claimVolume makes a volume usable by a machine on THIS host.
//
// Two hosts must never hold one volume, because they would both mount its
// SQLite metadata database and destroy it. So the row's owner is checked
// first, and taking it from another host is only allowed when that host has
// stopped heartbeating -- which the store re-verifies at the write.
func (m *Manager) claimVolume(ctx context.Context, volumeID, machineID string) (*state.Volume, error) {
	if m.opts.Volumes == nil {
		return nil, ErrNoVolumes
	}

	v, err := m.opts.Store.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if v.MachineID != "" && v.MachineID != machineID {
		return nil, fmt.Errorf("machines: volume %s is already attached to %s",
			volumeID, v.MachineID)
	}

	var opts []state.WriteOption
	if v.HostID != "" && v.HostID != m.opts.HostID {
		// The rescue case. The store refuses this unless the owner really has
		// stopped heartbeating, so a partitioned host that comes back cannot
		// find its volume mounted twice.
		opts = append(opts, state.WithDeadOwnerClaim(v.HostID))
	}

	v.MachineID = machineID
	v.HostID = m.opts.HostID
	if err := m.opts.Store.PutVolume(ctx, v, opts...); err != nil {
		return nil, err
	}

	// Only now: the row says this host owns it, so no other host will mount it
	// while this one does.
	if err := m.opts.Volumes.Attach(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

// releaseVolume gives a volume up when its machine is destroyed.
//
// Best effort on the filesystem side, exact on the row: a volume still marked
// as this host's is a volume no other host will mount, so the row has to be
// cleared even if the unmount failed.
func (m *Manager) releaseVolume(ctx context.Context, volumeID string) error {
	if volumeID == "" || m.opts.Volumes == nil {
		return nil
	}
	var errs []error
	if err := m.opts.Volumes.Detach(ctx, volumeID); err != nil {
		errs = append(errs, err)
	}
	if v, err := m.opts.Store.GetVolume(ctx, volumeID); err == nil {
		v.MachineID = ""
		if err := m.opts.Store.PutVolume(ctx, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// createWithVolume brings up a machine that has a volume attached.
//
// It BOOTS rather than restoring, and that is the one place in the engine
// where a create costs a kernel boot. The drive set is baked into a snapshot:
// Firecracker requires every backing file to be at the same path on restore,
// and there is no documented way to add a drive between loading a snapshot and
// resuming it. A template captured with a volume drive would not help either,
// since the drive's capacity travels with the device state and every volume is
// a different size.
//
// So the trade is explicit: a machine with a volume pays ~20s on its FIRST
// start, and every wake after that is an ordinary restore from its own
// snapshot, which does carry the drive.
func (m *Manager) createWithVolume(ctx context.Context, row *state.Machine,
	token, volumeID string) (*fc.Machine, error) {

	v, err := m.claimVolume(ctx, volumeID, row.ID)
	if err != nil {
		return nil, err
	}

	// The disk template this machine's later snapshots are diffs against. Its
	// root filesystem is a copy of the golden rootfs, so the golden rootfs's
	// build is the right and only parent.
	t, err := m.EnsureTemplate(ctx)
	if err != nil {
		return nil, err
	}
	row.TemplateRootfsBuildID = t.RootfsBuildID.String()
	// Its MEMORY has no parent, and saying so explicitly is load-bearing. This
	// guest booted; it is not a divergence from the template's photographed
	// memory, and diffing it against that template would record every
	// coincidentally-identical page as a pointer into a completely different
	// machine's memory image. Nothing would error -- the restore would just
	// hand the guest another machine's pages.
	row.TemplateMemBuildID = uuid.Nil.String()
	row.VolumeID = v.ID

	slot, err := m.pool.Take(row.ID)
	if err != nil {
		return nil, err
	}
	mac, err := fc.GenerateMAC()
	if err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}
	if err := netns.Setup(slot, mac, m.opts.FCConfig.JailUID); err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}

	cfg := m.machineFCConfig(row, slot, mac)
	cfg.VolumeImage = m.opts.Volumes.ImagePath(v.ID)

	fcm, err := fc.Boot(ctx, cfg)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("machines: boot %s with volume %s: %w", row.ID, v.ID, err)
	}

	if err := m.installToken(ctx, slot, token); err != nil {
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}
	if err := m.mountVolumeInGuest(ctx, slot, row.ID, v.MountPath); err != nil {
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, err
	}

	if err := fcm.Persist(); err != nil {
		slog.Error("booted machine's breadcrumbs were not written",
			"machine", row.ID, "err", err)
	}
	return fcm, nil
}

// mountVolumeInGuest tells the guest agent to mount the volume.
//
// Delivered rather than baked into the image. The mount path is per machine,
// and per-machine anything inside the golden template is wrong for every
// machine created from it. Fatal rather than best effort: a machine whose
// volume did not mount is writing to its ephemeral root filesystem while
// reporting that it has durable storage, which is worse than not starting.
func (m *Manager) mountVolumeInGuest(ctx context.Context, slot *netns.Slot,
	machineID, mountPath string) error {

	if mountPath == "" {
		mountPath = volumes.DefaultMountPath
	}
	if err := waitForAgent(ctx, slot.AgentAddr(), 60*time.Second); err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]string{
		"device": fc.GuestVolumeDevice, "mount_path": mountPath,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+slot.AgentAddr()+"/volume", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token(machineID))

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("machines: mount the volume in %s: %w", machineID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("machines: mount the volume in %s: status %d",
			machineID, resp.StatusCode)
	}
	return nil
}
