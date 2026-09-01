package volumes

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Create makes a new volume and leaves it mounted on this host, ready to be
// attached to a machine.
//
// The order is the design. The filesystem is formatted before anything
// replicates, replication starts before the first mount so that no window of
// writes goes unreplicated, and the image is created last -- because it is the
// first thing whose bytes matter, and everything that keeps those bytes alive
// has to already be running.
func (m *Manager) Create(ctx context.Context, name string, sizeMiB int, mountPath string) (*state.Volume, error) {
	if sizeMiB <= 0 {
		return nil, fmt.Errorf("volumes: a volume needs a size")
	}
	if mountPath == "" {
		mountPath = DefaultMountPath
	}
	if err := validateMountPath(mountPath); err != nil {
		return nil, err
	}

	id := NewID()
	if err := m.ensureDirs(id); err != nil {
		return nil, err
	}
	if err := m.writeConfig(id); err != nil {
		return nil, err
	}
	if err := m.format(ctx, id); err != nil {
		return nil, err
	}
	if err := m.startReplication(ctx, id); err != nil {
		return nil, err
	}
	if err := m.mount(ctx, id); err != nil {
		return nil, err
	}
	if err := m.createImage(ctx, id, sizeMiB); err != nil {
		return nil, err
	}

	return &state.Volume{
		ID: id, Name: name, SizeMiB: sizeMiB,
		S3Prefix: S3Prefix(id), MountPath: mountPath,
		HostID: m.cfg.HostID, CreatedAt: time.Now().Unix(),
	}, nil
}

// Attach makes an existing volume usable on THIS host.
//
// Called on the host a machine is being created, woken or rescued on. It is
// safe to call when the volume is already mounted here, and it is never safe
// to call while another host has it mounted -- which is what the volume row's
// single owner exists to prevent, since two JuiceFS mounts against one SQLite
// metadata database destroy it.
//
// The metadata is restored from object storage before the mount, every time.
// See restoreMeta: a stale local copy mounts without complaint and silently
// loses whatever the previous host wrote.
func (m *Manager) Attach(ctx context.Context, v *state.Volume) error {
	if err := m.ensureDirs(v.ID); err != nil {
		return err
	}
	if mounted, err := m.Mounted(v.ID); err != nil {
		return err
	} else if mounted {
		return nil
	}
	if err := m.writeConfig(v.ID); err != nil {
		return err
	}
	if err := m.restoreMeta(ctx, v.ID); err != nil {
		return err
	}
	if err := m.startReplication(ctx, v.ID); err != nil {
		return err
	}
	if err := m.mount(ctx, v.ID); err != nil {
		return err
	}
	if _, err := os.Stat(m.ImagePath(v.ID)); err != nil {
		return fmt.Errorf("volumes: %s mounted but has no image at %s -- its "+
			"metadata and its chunks disagree: %w", v.ID, m.ImagePath(v.ID), err)
	}
	return nil
}

// Detach releases a volume so another host can take it.
//
// Unmount first, then stop replicating. Stopping Litestream is what flushes
// the last metadata writes -- it syncs on a graceful shutdown -- so doing it
// before the unmount would leave the changes the unmount itself makes stranded
// on a host that is about to stop being the owner.
//
// The caller must have destroyed the machine first. Unmounting under a live
// guest loses writes silently rather than failing.
func (m *Manager) Detach(ctx context.Context, id string) error {
	if err := m.Unmount(ctx, id); err != nil {
		return err
	}
	return m.stopReplication(ctx, id)
}

// createImage lays down the raw ext4 image the guest gets as /dev/vdb.
//
// Sparse: truncate allocates nothing, and JuiceFS stores only the blocks that
// are actually written, so a 100 GiB volume costs what its data costs.
//
// mke2fs here rather than in the guest, so a volume that cannot be formatted
// fails the create rather than failing the first boot of a machine that
// already has a URL. The guest still checks for a superblock and formats one
// if it is missing, which covers a volume whose image was created by an older
// hostd or by a create that died between these two steps.
func (m *Manager) createImage(ctx context.Context, id string, sizeMiB int) error {
	path := m.ImagePath(id)
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("volumes: create %s: %w", path, err)
	}
	if err := f.Truncate(int64(sizeMiB) * 1024 * 1024); err != nil {
		f.Close()
		return fmt.Errorf("volumes: size %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("volumes: close %s: %w", path, err)
	}

	// 4KiB blocks, matching the guest page size and the block size everything
	// else in this engine addresses in.
	if _, err := m.run(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096",
		path, strconv.Itoa(sizeMiB)+"M"); err != nil {
		return fmt.Errorf("volumes: format the image for %s: %w", id, err)
	}
	return nil
}

// validateMountPath rejects a mount path that would break the guest.
//
// It is a path inside the guest that the agent will mount over, so it has to
// be absolute and it must not be one of the directories the system needs to
// already be there. Mounting a fresh empty filesystem over / or /etc produces
// a machine that boots and then cannot do anything, with nothing to point at.
func validateMountPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("volumes: mount path %q must be absolute", p)
	}
	clean := strings.TrimRight(p, "/")
	for _, reserved := range []string{"", "/bin", "/boot", "/dev", "/etc", "/lib",
		"/proc", "/run", "/sbin", "/sys", "/usr", "/var"} {
		if clean == reserved {
			return fmt.Errorf("volumes: mount path %q would shadow a directory the "+
				"guest needs to boot", p)
		}
	}
	return nil
}
