package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

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

// mountVolumeInGuest tells the guest agent to mount the volume.
//
// Delivered rather than baked into the image. The mount path is per machine,
// and per-machine anything inside the golden template is wrong for every
// machine created from it. Fatal rather than best effort: a machine whose
// volume did not mount is writing to its ephemeral root filesystem while
// reporting that it has durable storage, which is worse than not starting.
// The token is passed in rather than looked up with Manager.token: this runs
// inside the create, before the machine's credential has been remembered, and
// on a host with no fleet-wide AgentTokenSecret the lookup would fall back to
// the template placeholder the guest has just stopped accepting -- a 401 that
// fails every create with a volume.
func (m *Manager) mountVolumeInGuest(ctx context.Context, slot *netns.Slot,
	machineID, token, mountPath string) error {

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
	req.Header.Set("Authorization", "Bearer "+token)

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

// MachineVolume reports the volume drive a running machine actually has.
//
// The cache type is read back out of Firecracker rather than repeated from
// what this process meant to configure, and that is the point of the endpoint.
// Firecracker's default does not advertise the VirtIO flush feature, so a
// guest fsync to a drive left at the default returns success with the data
// only in the host's page cache. Nothing errors, nothing logs, and the
// durability the volume exists for is simply not happening -- so the value
// that gets checked has to be the one the VMM holds.
func (m *Manager) MachineVolume(ctx context.Context, machineID string) (*api.MachineVolume, error) {
	row, err := m.opts.Store.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if row.VolumeID == "" {
		return nil, fmt.Errorf("machines: %s has no volume: %w", machineID, state.ErrNotFound)
	}

	out := &api.MachineVolume{VolumeID: row.VolumeID, Device: fc.GuestVolumeDevice}
	if v, err := m.opts.Store.GetVolume(ctx, row.VolumeID); err == nil {
		out.MountPath = v.MountPath
	}

	fcm, ok := m.get(machineID)
	if !ok {
		// Suspended or owned elsewhere: there is no VMM to ask, and reporting
		// the intended value here would be exactly the lie this exists to
		// catch. The field stays empty.
		return out, nil
	}
	drive, err := fcm.Client.DriveConfig(ctx, fc.VolumeDriveID)
	if err != nil {
		return nil, fmt.Errorf("machines: read the volume drive of %s: %w", machineID, err)
	}
	out.CacheType = drive.CacheType
	return out, nil
}
