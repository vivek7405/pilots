package machines

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
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

// imageOnce serialises materialising one image. Two concurrent creates from a
// fresh build would otherwise each write the same multi-gigabyte file, and the
// second's rename would land under a name the first was still copying from.
var imageOnce sync.Map // build id -> *sync.Mutex

// imagesDir holds materialised rootfs images, one per build id. A cache: it
// can be deleted at any time and costs a re-materialise, never data.
func (m *Manager) imagesDir() string { return filepath.Join(m.opts.CacheRoot, "images") }

// imageForBuild returns a local ext4 file holding a build's contents.
//
// Cached per build id, because the file is then reflink-copied once per
// machine -- exactly the way the golden rootfs is. The first machine from a
// build pays for the materialise; every one after that pays a metadata
// operation.
func (m *Manager) imageForBuild(ctx context.Context, buildID uuid.UUID) (string, error) {
	path := filepath.Join(m.imagesDir(), buildID.String()+".ext4")

	lock, _ := imageOnce.LoadOrStore(buildID.String(), &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(m.imagesDir(), 0o755); err != nil {
		return "", fmt.Errorf("machines: image cache dir: %w", err)
	}

	// Pull the build local first. It is content-addressed and already in
	// object storage, so this is a download, and it lands in exactly the
	// layout every other build directory has -- which is what lets this
	// machine's later disk diffs resolve against it.
	if err := m.materializeBuild(ctx, buildID); err != nil {
		return "", fmt.Errorf("machines: fetch build %s: %w", buildID, err)
	}
	built, err := block.OpenLocalBuild(filepath.Join(m.buildDir(), buildID.String()))
	if err != nil {
		return "", fmt.Errorf("machines: open build %s: %w", buildID, err)
	}
	defer built.Close()

	if err := block.Materialize(ctx, built, path); err != nil {
		return "", fmt.Errorf("machines: materialize build %s: %w", buildID, err)
	}
	return path, nil
}

// bootMachine starts a machine that cannot be restored from the golden
// template: one with its own disk image, one with a volume, or both.
//
// Both cases pay a real kernel boot, and both pay it exactly once. The
// machine's first suspend captures its own memory, and every wake after that
// is the ordinary instant restore.
func (m *Manager) bootMachine(ctx context.Context, row *state.Machine,
	token, volumeID, image, appCmd string) (*fc.Machine, error) {

	rootfs := m.opts.FCConfig.TemplateRootfs
	initPath := ""

	if image != "" {
		buildID, err := uuid.Parse(image)
		if err != nil {
			return nil, fmt.Errorf("machines: %q is not a build id: %w", image, err)
		}
		if rootfs, err = m.imageForBuild(ctx, buildID); err != nil {
			return nil, err
		}
		row.ImageRef = image
		// The kernel is told what to run, rather than the image being edited
		// to say it: a base image that ships its own init keeps it, and tar
		// cannot override an existing /sbin/init symlink anyway.
		initPath = build.AgentPathInImage
		// The build IS this machine's disk template. Its later snapshots are
		// diffs whose unchanged ranges resolve against it by offset, and the
		// bytes it booted from are exactly this build's -- so pinning anything
		// else here, the golden template included, hands a restored guest
		// another image's blocks.
		row.TemplateRootfsBuildID = image
	} else {
		t, err := m.EnsureTemplate(ctx)
		if err != nil {
			return nil, err
		}
		row.TemplateRootfsBuildID = t.RootfsBuildID.String()
	}

	// No memory parent, and recorded explicitly rather than left empty. This
	// guest booted; its pages are not a divergence from any template's
	// photographed memory, and diffing against one would resolve every
	// coincidentally identical page from a completely different machine.
	row.TemplateMemBuildID = uuid.Nil.String()

	var vol *state.Volume
	if volumeID != "" {
		v, err := m.claimVolume(ctx, volumeID, row.ID)
		if err != nil {
			return nil, err
		}
		vol = v
		row.VolumeID = v.ID
	}

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
	cfg.TemplateRootfs = rootfs
	cfg.InitPath = initPath

	fcm, err := fc.Boot(ctx, cfg)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("machines: boot %s: %w", row.ID, err)
	}

	if err := m.installToken(ctx, slot, token); err != nil {
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}
	if vol != nil {
		if err := m.mountVolumeInGuest(ctx, slot, row.ID, token, vol.MountPath); err != nil {
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
	if err := m.deliverEnv(ctx, row, slot, appCmd); err != nil {
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
