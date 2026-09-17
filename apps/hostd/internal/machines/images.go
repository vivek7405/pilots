package machines

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/build"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Creating a machine from something a build produced.
//
// The build path deliberately emits a generation-0 template build -- the same
// artifact the golden template's disk is -- so nothing here knows what a
// Dockerfile is. What it does know is that a machine started from a NEW disk
// has to boot rather than restore: the golden template's memory image
// describes the golden template's disk, and resuming it against somebody
// else's root filesystem is a guest whose memory and disk have never met.

// RemoveLegacyImageCache deletes the materialised image cache an earlier hostd
// kept under the cache root: one full-size ext4 per build, which nothing ever
// deleted and which filled a rig host's disk.
//
// Nothing writes it any more -- a booted machine's root is served from its
// build -- so this is not a collector with a job, it is the one removal a
// host upgraded in place needs to stop carrying those gigabytes forever. A
// fresh host has nothing here and this is a no-op.
func (m *Manager) RemoveLegacyImageCache() {
	dir := filepath.Join(m.opts.CacheRoot, "images")
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("could not remove the legacy image cache; nothing writes it, "+
			"and it can be deleted by hand", "dir", dir, "err", err)
		return
	}
	slog.Info("removed the legacy materialised image cache", "dir", dir)
}

// pinBootTemplate decides which build a booting machine's root is served from
// and records that decision on the row.
//
// The disk is served over NBD straight from the build, exactly as a restored
// machine's is: there is no per-machine rootfs file, so a host's disk holds
// one copy of a build however many machines boot it, and a host that has
// never seen the build pulls it from object storage rather than from another
// host. The row is pinned to the same build the device reads, so the
// machine's later disk diffs resolve against the bytes it actually booted.
func (m *Manager) pinBootTemplate(ctx context.Context, row *state.Machine,
	image string) (fc.Backends, error) {

	backends := fc.Backends{CacheRoot: m.buildDir()}

	if image != "" {
		buildID, err := uuid.Parse(image)
		if err != nil {
			return backends, fmt.Errorf("machines: %q is not a build id: %w", image, err)
		}
		// The build's header now, its bytes in the background: the block
		// server serves the guest from object storage while the local copy
		// hydrates, and everything that needs the copy complete -- a flush,
		// a checkpoint -- waits for the marker rather than for this create.
		// It still lands in the layout every other build directory has,
		// which is what lets this machine's later disk diffs resolve against
		// it. A foreground pull here held every replica placed on a host
		// that had never seen its image for the whole download.
		if err := m.materializeTemplate(ctx, uuid.Nil, buildID); err != nil {
			return backends, fmt.Errorf("machines: fetch build %s: %w", buildID, err)
		}
		backends.RootfsTemplateDir = filepath.Join(m.buildDir(), buildID.String())
		backends.RootfsTemplateID = buildID
		row.ImageRef = image
		// The build IS this machine's disk template. Its later snapshots are
		// diffs whose unchanged ranges resolve against it by offset, and the
		// bytes it booted from are exactly this build's -- so pinning anything
		// else here, the golden template included, hands a restored guest
		// another image's blocks.
		row.TemplateRootfsBuildID = image
	} else {
		t, err := m.EnsureTemplate(ctx)
		if err != nil {
			return backends, err
		}
		backends.RootfsTemplateDir = m.rootfsTemplateDir(t)
		backends.RootfsTemplateID = t.RootfsBuildID
		row.TemplateRootfsBuildID = t.RootfsBuildID.String()
	}

	// No memory parent, and recorded explicitly rather than left empty. This
	// guest booted; its pages are not a divergence from any template's
	// photographed memory, and diffing against one would resolve every
	// coincidentally identical page from a completely different machine.
	row.TemplateMemBuildID = uuid.Nil.String()
	return backends, nil
}

// bootMachine starts a machine that cannot be restored from the golden
// template: one with its own disk image, one with a volume, or both.
//
// Both cases pay a real kernel boot, and both pay it exactly once. The
// machine's first suspend captures its own memory, and every wake after that
// is the ordinary instant restore.
func (m *Manager) bootMachine(ctx context.Context, row *state.Machine,
	token, volumeID, image, appCmd string) (*fc.Machine, error) {

	backends, err := m.pinBootTemplate(ctx, row, image)
	if err != nil {
		return nil, err
	}
	initPath := ""
	if image != "" {
		// The kernel is told what to run, rather than the image being edited
		// to say it: a base image that ships its own init keeps it, and tar
		// cannot override an existing /sbin/init symlink anyway.
		initPath = build.AgentPathInImage
	}

	var vol *state.Volume
	if volumeID != "" {
		v, err := m.claimVolume(ctx, volumeID, row.ID)
		if err != nil {
			return nil, err
		}
		vol = v
		row.VolumeID = v.ID
	}

	slot, err := m.takeSlot(row)
	if err != nil {
		return nil, err
	}
	mac, err := fc.GenerateMAC()
	if err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}

	// No netns.Setup here: BootFromDisk does it, teardown-first, alongside
	// starting the block server.
	cfg := m.machineFCConfig(row, slot, mac)
	cfg.InitPath = initPath

	boot := fc.InstantConfig{
		Config:   cfg,
		Backends: backends,
		Env:      m.opts.HandlerEnv,
	}
	boot.ChunksSock = m.chunks.start(row.ID, boot.StateDir,
		m.opts.BlockStore, allowedBuilds(boot))

	fcm, err := fc.BootFromDisk(ctx, boot, m.opts.BlockStore, m.opts.NBDDevices)
	if fcm != nil {
		m.joinHandlersToCgroup(fcm)
	}
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("machines: boot %s: %w", row.ID, err)
	}

	// A booted machine needs the responder exactly as much as a restored one.
	// The restore path binds inside m.restore, which this path never touches
	// -- and the golden rootfs names the gateway as its ONLY nameserver, so a
	// machine that boots it with nothing listening there resolves nothing at
	// all, .internal and the public internet alike.
	m.bindDiscovery(row.ID, slot)

	if err := m.installToken(ctx, slot, token); err != nil {
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}
	if vol != nil {
		if err := m.mountVolumeInGuest(ctx, slot, row.ID, token, vol.MountPath); err != nil {
			m.releaseDiscovery(row.ID)
			_ = fcm.Kill()
			m.pool.Return(slot.Idx)
			return nil, err
		}
	}

	// The environment goes in after the volume is mounted, because an
	// application started here may expect to find its data already there.
	//
	// A machine that booted needs this exactly as much as one that restored:
	// it is a create either way, and it is the only moment an application can
	// be handed an environment it did not start with. An image built from a
	// Dockerfile carries no command in the row at all, so appCmd is usually
	// empty here and the start spec baked into the image supplies it.
	if err := m.deliverEnv(ctx, row, slot, appCmd, true); err != nil {
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("deliver env: %w", err)
	}

	if err := fcm.Persist(); err != nil {
		// Not fatal: the machine is running and serving. But a restart will
		// not re-adopt it, which is worth shouting about.
		slog.Error("booted machine's breadcrumbs were not written",
			"machine", row.ID, "err", err)
	}
	return fcm, nil
}
