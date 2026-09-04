package fc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// The instant path replaces Phase 2's whole-file transport. Nothing is copied
// up front: the guest's memory and disk are both served on demand out of a
// content-addressed store, so a restore costs the snapshot's metadata plus
// whatever the guest actually touches -- not the size of the machine.
//
// Both handlers run as their OWN processes, re-executed from this binary. That
// is what lets hostd be restarted without taking every running machine down
// with it: a Firecracker whose block server or fault server has vanished
// blocks in an uninterruptible wait that no signal clears.

// Backends names the content-addressed artifacts a machine restores against.
type Backends struct {
	// MemBuildID is this machine's memory image. MemParentBuildID is the
	// template it was diffed against, and is required whenever the memory
	// build is a diff rather than a template.
	MemBuildID       uuid.UUID
	MemParentBuildID uuid.UUID

	// RootfsTemplateDir is the golden template, on local disk. Every machine
	// reads through the same one.
	RootfsTemplateDir string
	// RootfsTemplateID reads that template from object storage instead, for a
	// host that does not have it yet.
	RootfsTemplateID uuid.UUID
	// RootfsDiffID replays a previous lifetime's disk writes before the guest
	// reads anything.
	RootfsDiffID uuid.UUID

	CacheRoot string
}

// InstantConfig describes a lazy restore.
type InstantConfig struct {
	Config
	Backends
	// LocalDir caches the snapshot metadata, which is small and is the only
	// artifact still fetched whole.
	LocalDir   string
	AgentToken string
	// SnapKey is the object-storage key of the Firecracker vmstate file.
	SnapKey string
	// SnapImmutable marks a vmstate written once under its own key -- a
	// checkpoint -- so a local copy can be trusted. A machine's suspend image
	// is the opposite: one key, rewritten on every suspend, so a cached copy
	// silently restores the PREVIOUS suspend and loses everything since.
	SnapImmutable bool
	// Env is handed to the handler processes; they need the storage
	// credentials.
	Env []string
}

// CowPath is where a machine's copy-on-write disk lives.
func CowPath(stateDir string) string { return nbd.CachePathFor(stateDir) }

// RestoreInstant brings a machine back with both backends served lazily.
//
// The namespace, the two handlers and the snapshot fetch all proceed at once.
// They are genuinely independent, and the namespace is the only one the jailer
// waits on -- it joins the namespace at exec time.
func RestoreInstant(ctx context.Context, cfg InstantConfig, dl Uploader,
	store block.ObjectStore, pool *nbd.DevicePool) (m *Machine, err error) {

	if err := os.MkdirAll(cfg.LocalDir, 0o755); err != nil {
		return nil, fmt.Errorf("fc: mkdir restore dir: %w", err)
	}
	if err := checkChrootBaseUsable(cfg.ChrootBase); err != nil {
		return nil, err
	}

	chrootDir := ChrootDir(cfg.ChrootBase, cfg.FirecrackerBin, cfg.MachineID)
	for _, dir := range []string{
		chrootDir,
		filepath.Join(chrootDir, filepath.Dir(BakedRootfsPath)),
		filepath.Join(chrootDir, "run"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("fc: mkdir %s: %w", dir, err)
		}
	}

	// The machine's state directory is created here rather than assumed: on a
	// create it does not exist yet, and this is the first thing to write into
	// it.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("fc: mkdir state dir: %w", err)
	}

	logFile, err := os.OpenFile(filepath.Join(cfg.StateDir, "handlers.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fc: open handler log: %w", err)
	}
	defer logFile.Close()

	// Firecracker is chrooted, so it can only reach the socket and the device
	// through paths INSIDE the jail. Both therefore live in the chroot and are
	// named to Firecracker relative to it.
	uffdSock := filepath.Join(chrootDir, "uffd.sock")
	localSnap := filepath.Join(cfg.LocalDir, SnapFile)

	var (
		nbdProc  *nbd.Process
		uffdProc *uffd.Process
	)
	// Anything already started has to be cleaned up if a later step fails, or
	// the host leaks an attached device and a live handler per failed restore.
	defer func() {
		if err == nil {
			return
		}
		if uffdProc != nil {
			_ = uffdProc.Stop()
		}
		if nbdProc != nil {
			_ = nbdProc.Stop()
		}
	}()

	err = inParallel(
		func() error {
			// Teardown-first, so re-creating an existing namespace is safe.
			// Restore owns this rather than assuming it survived: a suspend
			// releases the namespace, and a restore on another host never had
			// one.
			if err := netns.Setup(cfg.Slot, cfg.MAC, cfg.JailUID); err != nil {
				return fmt.Errorf("fc: netns for restore: %w", err)
			}
			return nil
		},
		func() error {
			var perr error
			nbdProc, perr = nbd.Start(ctx, pool, nbd.StartOptions{
				Config: nbd.Config{
					TemplateDir:      cfg.RootfsTemplateDir,
					TemplateBuildID:  cfg.RootfsTemplateID,
					RehydrateBuildID: cfg.RootfsDiffID,
					CachePath:        CowPath(cfg.StateDir),
					ControlSock:      nbd.ControlSockFor(cfg.StateDir),
					CacheRoot:        cfg.Backends.CacheRoot,
				},
				Env: cfg.Env, LogFile: logFile,
			})
			return perr
		},
		func() error {
			var perr error
			uffdProc, perr = uffd.Start(ctx, uffd.StartOptions{
				Config: uffd.Config{
					Socket:        uffdSock,
					BuildID:       cfg.MemBuildID,
					ParentBuildID: cfg.MemParentBuildID,
					CacheRoot:     cfg.Backends.CacheRoot,
					PrefetchFile:  uffd.PrefetchFor(cfg.StateDir),
					ControlSock:   uffd.ControlSockFor(cfg.StateDir),
				},
				Env: cfg.Env, LogFile: logFile,
			})
			return perr
		},
		func() error {
			// The vmstate file is the one artifact still fetched whole. It is
			// kilobytes: device state and vcpu registers, not memory.
			if cfg.SnapImmutable {
				return fetchIfAbsent(ctx, dl, cfg.SnapKey, localSnap)
			}
			return dl.GetToFile(ctx, cfg.SnapKey, localSnap)
		},
	)
	if err != nil {
		return nil, err
	}

	if err = nbd.WaitReady(nbdProc.Index, 0); err != nil {
		return nil, err
	}
	if err = stageInstantJail(cfg, chrootDir, localSnap, nbdProc.Device, uffdSock); err != nil {
		return nil, err
	}

	serialLog := filepath.Join(cfg.StateDir, "lifecycle.log")
	serial, err := os.OpenFile(serialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fc: open serial log: %w", err)
	}
	defer serial.Close()

	cmd := exec.CommandContext(context.Background(), cfg.JailerBin, jailerArgs(cfg.Config)...)
	cmd.Stdout = serial
	cmd.Stderr = serial
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("fc: start jailer for restore: %w", err)
	}

	m = &Machine{
		ID:        cfg.MachineID,
		MemMiB:    cfg.MemMiB,
		Slot:      cfg.Slot,
		Cmd:       cmd,
		Client:    NewClient(filepath.Join(chrootDir, "run", "fc.sock")),
		ChrootDir: chrootDir,
		StateDir:  cfg.StateDir,
		SerialLog: serialLog,
		StartedAt: time.Now(),
		NBD:       nbdProc,
		Uffd:      uffdProc,
	}
	// From here the machine owns the handlers, so the cleanup above must not
	// also stop them.
	nbdProc, uffdProc = nil, nil
	defer func() {
		if err != nil {
			_ = m.Kill()
			m = nil
		}
	}()

	if err = m.Client.WaitForSocket(ctx, 60*time.Second); err != nil {
		return nil, err
	}

	if err = m.Client.LoadSnapshot(ctx, SnapshotLoad{
		SnapshotPath: "/" + SnapFile,
		// Uffd, not File: Firecracker sends the fault descriptor to this
		// socket and resumes without reading a byte of the memory image.
		MemBackend: MemBackend{BackendType: "Uffd", BackendPath: "/uffd.sock"},
		ResumeVM:   false,
	}); err != nil {
		return nil, fmt.Errorf("fc: load snapshot %s: %w", cfg.MachineID, err)
	}

	if err = m.Client.Resume(ctx); err != nil {
		return nil, fmt.Errorf("fc: resume %s: %w", cfg.MachineID, err)
	}

	go pokeGuestClock(cfg.Slot, cfg.AgentToken, m.ID)
	return m, nil
}

// stageInstantJail puts the snapshot, the disk device and the fault socket
// where the jailed Firecracker can reach them.
func stageInstantJail(cfg InstantConfig, chrootDir, snap, device, uffdSock string) error {
	if err := reflinkCopy(snap, filepath.Join(chrootDir, SnapFile)); err != nil {
		return err
	}

	// The disk is a device node, not a file. It goes at the same baked path a
	// Phase 2 rootfs did, because that path is recorded inside every snapshot
	// -- which is what keeps a snapshot host-agnostic.
	jailRootfs := filepath.Join(chrootDir, BakedRootfsPath)
	if err := mknodBlockDevice(device, jailRootfs, cfg.JailUID, cfg.JailGID); err != nil {
		return err
	}

	// The volume is staged but NOT re-declared to Firecracker. Its drive is
	// already inside the snapshot being loaded -- the machine was created from
	// a template captured with the drive attached -- and a restore has no
	// window in which to add one: there is no documented way to touch the
	// drive set between /snapshot/load and PATCH /vm Resumed. What the restore
	// owes it is a file at the baked path, which is what this is.
	if err := stageVolume(chrootDir, cfg.VolumeImage, cfg.JailUID, cfg.JailGID); err != nil {
		return err
	}

	for _, p := range []string{
		chrootDir,
		filepath.Join(chrootDir, "run"),
		filepath.Join(chrootDir, SnapFile),
		filepath.Dir(jailRootfs),
		uffdSock,
	} {
		if err := os.Chown(p, cfg.JailUID, cfg.JailGID); err != nil {
			return fmt.Errorf("fc: chown %s: %w", p, err)
		}
	}
	return nil
}

// mknodBlockDevice recreates a block device inside the jail.
//
// Firecracker cannot open /dev/nbdN directly: the jailer chroots it, and the
// jailer only creates the handful of nodes it knows about. Copying the major
// and minor here gives the same device a second name that the jailed process
// can reach.
func mknodBlockDevice(device, dest string, uid, gid int) error {
	var st unix.Stat_t
	if err := unix.Stat(device, &st); err != nil {
		return fmt.Errorf("fc: stat %s: %w", device, err)
	}

	// A previous lifetime's node may still be there, pointing at a device that
	// now belongs to a different machine.
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fc: remove stale device node: %w", err)
	}
	if err := unix.Mknod(dest, unix.S_IFBLK|0o660, int(st.Rdev)); err != nil {
		return fmt.Errorf("fc: mknod %s for %s: %w", dest, device, err)
	}
	if err := os.Chown(dest, uid, gid); err != nil {
		return fmt.Errorf("fc: chown device node: %w", err)
	}
	return nil
}

// InstantSnapshot is what a chunkified snapshot produced.
type InstantSnapshot struct {
	MemBuildID    uuid.UUID
	RootfsBuildID uuid.UUID // uuid.Nil when the machine never wrote to disk

	// ResumeGap is how long the guest was actually frozen.
	//
	// Reported rather than left to be inferred from the call's duration: the
	// preparation before the pause -- waiting for the previous capture,
	// making memory resident -- happens with the machine running and serving,
	// so the round trip overstates the freeze, sometimes by a lot. An agent
	// checkpointing between messages needs the number that describes what its
	// user experiences.
	ResumeGap time.Duration
}

// SnapshotOpts describes where a chunkified snapshot should land.
type SnapshotOpts struct {
	// MemParentDir is the memory template this snapshot is diffed against. An
	// empty value produces a self-contained template.
	MemParentDir string
	// RootfsTemplateDir is the disk template the copy-on-write file is diffed
	// against. Always set: a CoW file is meaningless without it.
	RootfsTemplateDir string
	// BuildDir is where the produced builds are written before upload.
	BuildDir string
}

// Chunkify captures the machine's current state as content-addressed builds.
//
// The guest must be PAUSED. The dirty bitmap and the memory image both
// describe an instant, and taking either while the guest runs produces a
// snapshot of a state that never existed.
//
// The rootfs is chunkified BEFORE the handlers are stopped, and that ordering
// is load-bearing: the bitmap of written blocks lives in the handler's memory,
// and once the process is gone there is no way to tell a block the guest
// zeroed from one it never touched.
func (m *Machine) Chunkify(ctx context.Context, opts SnapshotOpts) (InstantSnapshot, error) {
	var out InstantSnapshot

	p := m.snapshotPaths()

	memBuild := uuid.New()
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In:        p.hostMem,
		OutDir:    filepath.Join(opts.BuildDir, memBuild.String()),
		BuildID:   memBuild,
		ParentDir: opts.MemParentDir,
	}); err != nil {
		return out, fmt.Errorf("fc: chunkify memory: %w", err)
	}
	out.MemBuildID = memBuild

	if m.NBD == nil {
		// No block server means this machine was BOOTED rather than restored,
		// and its disk is a plain file in the jail. That happens for exactly
		// two machines: the throwaway one a golden template is photographed
		// from, which has no disk worth keeping and says so by leaving
		// RootfsTemplateDir empty, and a machine created with a volume, which
		// has to be booted because a drive cannot be added to a snapshot being
		// restored.
		//
		// The second one's disk very much matters. Skipping it here is silent:
		// the machine suspends, wakes from the template alone, and every write
		// it made to its root filesystem is gone with nothing reporting it.
		//
		// Diffing the file wholesale is correct here in a way it would not be
		// for a copy-on-write cache. This file is a full copy of the template,
		// not a sparse overlay, so every block holds real data and a block
		// that matches the parent genuinely is unchanged -- which is exactly
		// the ambiguity the dirty bitmap exists to resolve for a cow.
		if opts.RootfsTemplateDir == "" {
			return out, nil
		}
		bootedRootfs := filepath.Join(m.ChrootDir, BakedRootfsPath)
		if _, statErr := os.Stat(bootedRootfs); statErr != nil {
			return out, nil
		}

		rootfsBuild := uuid.New()
		if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
			In:        bootedRootfs,
			OutDir:    filepath.Join(opts.BuildDir, rootfsBuild.String()),
			BuildID:   rootfsBuild,
			ParentDir: opts.RootfsTemplateDir,
		}); err != nil {
			return out, fmt.Errorf("fc: chunkify booted disk: %w", err)
		}
		out.RootfsBuildID = rootfsBuild
		return out, nil
	}

	dirty, err := m.NBD.Dirty()
	if err != nil {
		return out, fmt.Errorf("fc: read dirty blocks: %w", err)
	}
	if dirty.IsEmpty() {
		// The machine never wrote to its disk. Its next restore reads the
		// template directly, so there is nothing to store.
		return out, nil
	}

	rootfsBuild := uuid.New()
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In:        CowPath(m.StateDir),
		OutDir:    filepath.Join(opts.BuildDir, rootfsBuild.String()),
		BuildID:   rootfsBuild,
		ParentDir: opts.RootfsTemplateDir,
		Dirty:     dirty,
	}); err != nil {
		return out, fmt.Errorf("fc: chunkify disk: %w", err)
	}
	out.RootfsBuildID = rootfsBuild
	return out, nil
}

// DirtyBlocks reports which disk blocks the machine has written.
func (m *Machine) DirtyBlocks() (*roaring.Bitmap, error) {
	if m.NBD == nil {
		return roaring.New(), nil
	}
	return m.NBD.Dirty()
}

// inParallel runs functions concurrently and joins their errors.
//
// All of them run to completion even when one fails: each may have started a
// process or a namespace that the caller has to be able to clean up, and a
// short circuit would leave those unaccounted for.
func inParallel(fns ...func() error) error {
	errs := make([]error, len(fns))

	var wg sync.WaitGroup
	wg.Add(len(fns))
	for i, fn := range fns {
		go func() {
			defer wg.Done()
			errs[i] = fn()
		}()
	}
	wg.Wait()

	return errors.Join(errs...)
}

// stopHandlers tears down a machine's block and fault servers.
//
// Order matters and runs opposite to startup. Firecracker must be gone first:
// it is the only thing issuing block I/O and taking page faults, and killing a
// server out from under a live guest leaves it in an uninterruptible wait that
// no signal clears.
func (m *Machine) stopHandlers() []error {
	var errs []error

	if m.Uffd != nil {
		if err := m.Uffd.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop uffd handler: %w", err))
		}
		m.Uffd = nil
	}
	if m.NBD != nil {
		if err := m.NBD.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop nbd handler: %w", err))
		}
		m.NBD = nil
	}
	return errs
}

// DiscardCow removes a machine's copy-on-write disk.
//
// Only on destroy. It holds every write since the last snapshot, so removing
// it at any other point silently discards the machine's disk.
func (m *Machine) DiscardCow() {
	if err := os.Remove(CowPath(m.StateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not remove copy-on-write file", "machine", m.ID, "err", err)
	}
}

// CowFile is the name a checkpoint's copy-on-write disk is staged under.
const CowFile = "rootfs.cow"

// InstantArtifacts names the objects one instant snapshot produced.
type InstantArtifacts struct {
	InstantSnapshot
	// SnapKey is where the Firecracker vmstate landed.
	SnapKey string
}

// SuspendInstant snapshots a machine into the chunk store and stops it.
//
// This is the scale-to-zero path: what remains is a few hundred kilobytes of
// vmstate plus the blocks the machine actually changed. The guest is not
// resumed -- the caller is deliberately giving up the memory.
//
// The ordering is the whole point of the function and every step earns its
// place. The disk is chunkified while the handlers are still alive, because
// the dirty bitmap only exists in the block server's memory. The vmstate is
// uploaded before the kill, because the kill removes the jail it lives in. The
// fault order is uploaded after, because the fault server writes it as it
// runs and it is only complete once that process is gone.
func (m *Machine) SuspendInstant(ctx context.Context, up Uploader, chunks Uploader,
	opts SnapshotOpts, snapKey, prefetchKey string) (res InstantArtifacts, err error) {

	res.SnapKey = snapKey

	// Same two reasons as a checkpoint, and in the same order: a capture still
	// running from an earlier checkpoint would compete with the snapshot
	// write, and pages that are not resident fault in with the guest frozen.
	// See CheckpointInstant.
	m.awaitCapture()
	// Only a Full snapshot needs this. Firecracker reads ALL of guest memory
	// to write one, so a page still lazily backed faults through the handler
	// with the guest frozen -- 5.8 seconds against 450ms on a 512MiB machine.
	//
	// A Diff must NOT get it. Without track_dirty_pages Firecracker's dirty
	// set is the RESIDENT set, so installing every page first turns the Diff
	// straight back into a Full and costs the whole lever.
	if m.snapshotType(m.snapshotPaths().hostMem, m.MemMiB) == SnapshotFull {
		m.makeMemoryResident()
	}

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		m.resumeAfterFailure(ctx, err)
		return res, err
	}
	// A failure between here and the kill leaves the guest frozen. A paused VM
	// whose row still says "running" is worse than a failed suspend: the
	// router proxies into a machine that can never answer, and the idle
	// monitor retries the same failing suspend on every tick.
	defer func() {
		if err != nil {
			m.resumeAfterFailure(ctx, err)
		}
	}()

	snapshot, err := m.Chunkify(ctx, opts)
	if err != nil {
		return res, err
	}
	res.InstantSnapshot = snapshot

	if err = uploadBuild(ctx, chunks, opts.BuildDir, snapshot.MemBuildID); err != nil {
		return res, err
	}
	if snapshot.RootfsBuildID != uuid.Nil {
		if err = uploadBuild(ctx, chunks, opts.BuildDir, snapshot.RootfsBuildID); err != nil {
			return res, err
		}
	}
	if err = up.PutFile(ctx, snapKey, p.hostSnap); err != nil {
		return res, err
	}

	// The memory image is durable now, and on a busy host these are the
	// largest files around.
	_ = os.Remove(p.hostMem)

	if err = m.Kill(); err != nil {
		return res, err
	}

	// Best effort: a machine with no recorded fault order simply wakes with a
	// cold cache, which is slower and not wrong.
	if prefetchKey != "" {
		prefetch := uffd.PrefetchFor(m.StateDir)
		if _, serr := os.Stat(prefetch); serr == nil {
			if perr := up.PutFile(ctx, prefetchKey, prefetch); perr != nil {
				slog.Warn("could not upload the fault order; the next wake will "+
					"be cold", "machine", m.ID, "err", perr)
			}
		}
	}
	return res, nil
}

// CheckpointInstant captures a restorable point and resumes immediately.
//
// The guest is frozen only long enough to write its vmstate, read the dirty
// bitmap, and reflink-copy two files -- all metadata operations on a
// reflink-capable filesystem, so the pause is roughly independent of how big
// the machine is. Chunkifying and uploading happen afterwards, with the
// machine already serving.
func (m *Machine) CheckpointInstant(ctx context.Context, up Uploader, chunks Uploader,
	opts SnapshotOpts, localDir, snapKey string) (res InstantSnapshot, err error) {

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return res, fmt.Errorf("fc: mkdir checkpoint dir: %w", err)
	}
	// This directory may be a retry of a failed attempt.
	for _, marker := range []string{durableMarker, failedMarker, chunkedMarker} {
		_ = os.Remove(filepath.Join(localDir, marker))
	}

	// Wait for the PREVIOUS checkpoint's background work to finish before
	// pausing.
	//
	// That work reads the whole memory image, chunkifies it and uploads it. It
	// runs perfectly happily alongside a running guest -- but if it is still
	// going when the next snapshot starts, it competes with Firecracker's
	// snapshot write, and that write is inside the pause. The freeze the user
	// feels then belongs to the checkpoint BEFORE the one they asked for.
	//
	// Waiting here rather than skipping it puts that delay where the guest is
	// still running and serving.
	m.awaitCapture()

	// Make the memory resident BEFORE pausing.
	//
	// Firecracker reads all of guest memory to write a snapshot, so every page
	// still lazily backed faults through the fault handler with the guest
	// frozen. On a machine's first checkpoint that is its entire memory, one
	// page at a time, inside the window the user experiences as a freeze --
	// measured at 5.8 seconds for 512MiB, against 450ms once resident.
	// Only a Full snapshot needs this. Firecracker reads ALL of guest memory
	// to write one, so a page still lazily backed faults through the handler
	// with the guest frozen -- 5.8 seconds against 450ms on a 512MiB machine.
	//
	// A Diff must NOT get it. Without track_dirty_pages Firecracker's dirty
	// set is the RESIDENT set, so installing every page first turns the Diff
	// straight back into a Full and costs the whole lever.
	if m.snapshotType(m.snapshotPaths().hostMem, m.MemMiB) == SnapshotFull {
		m.makeMemoryResident()
	}

	// The resume gap is the only part of a checkpoint a user experiences, so
	// it is measured rather than assumed.
	pausedAt := time.Now()

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		if rerr := m.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
			slog.Error("checkpoint failed and the guest could not be resumed",
				"machine", m.ID, "err", rerr)
		}
		return res, err
	}

	// Resume as soon as the local copies exist. Everything after runs with the
	// machine already serving.
	defer func() {
		res.ResumeGap = time.Since(pausedAt)
		metrics.SnapshotResumeGapSeconds.Observe(res.ResumeGap.Seconds())
		slog.Info("checkpoint resume gap", "machine", m.ID,
			"ms", res.ResumeGap.Milliseconds())
		if rerr := m.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
			slog.Error("guest could not be resumed after checkpoint",
				"machine", m.ID, "err", rerr)
			if err == nil {
				err = fmt.Errorf("fc: resume after checkpoint: %w", rerr)
			}
		}
	}()

	// Read the bitmap while the guest is still paused. It describes an
	// instant, and one taken while the guest writes describes a disk state
	// that never existed.
	dirty, err := m.DirtyBlocks()
	if err != nil {
		return res, err
	}

	// The build ids are minted HERE, not when the background work finishes.
	// The caller records them on the checkpoint row and returns immediately,
	// so a client can roll back to a checkpoint the moment it is chunkified
	// rather than waiting for the upload it does not need locally.
	res.MemBuildID = uuid.New()
	if !dirty.IsEmpty() {
		res.RootfsBuildID = uuid.New()
	}

	localSnap := filepath.Join(localDir, SnapFile)
	localCow := filepath.Join(localDir, CowFile)

	if err := reflinkCopy(p.hostSnap, localSnap); err != nil {
		return res, err
	}
	// The memory image is NOT copied. It is chunkified where Firecracker wrote
	// it, which is safe because awaitCapture above guarantees the previous
	// capture finished before Firecracker was allowed to overwrite it.
	//
	// Copying it first only looked free: a reflink of half a gigabyte pins its
	// extents, and on a copy-on-write filesystem that allocation pressure
	// comes back as a multi-second snapshot write a checkpoint or two later --
	// inside the pause. The copy protected nothing, either: a capture
	// interrupted by a crash leaves the checkpoint unusable whether or not a
	// staged copy survives, because nothing resumes one.
	if !dirty.IsEmpty() {
		if err := reflinkCopy(CowPath(m.StateDir), localCow); err != nil {
			return res, err
		}
	}

	m.beginCapture()
	go func() {
		defer m.endCapture()
		m.finishCheckpoint(up, chunks, opts, localDir, p.hostMem, snapKey, dirty, res)
	}()
	return res, nil
}

// finishCheckpoint chunkifies and uploads a checkpoint's staged copies.
func (m *Machine) finishCheckpoint(up Uploader, chunks Uploader, opts SnapshotOpts,
	localDir, memPath, snapKey string, dirty *roaring.Bitmap, ids InstantSnapshot) {

	uploadSlots <- struct{}{}
	defer func() { <-uploadSlots }()

	// Detached from any request context: a client that hangs up must not
	// abandon a half-uploaded checkpoint.
	ctx := context.Background()

	fail := func(err error) {
		slog.Error("checkpoint could not be completed", "machine", m.ID, "err", err)
		_ = os.WriteFile(filepath.Join(localDir, failedMarker), []byte(err.Error()), 0o644)
	}

	_, memStats, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In:      memPath,
		OutDir:  filepath.Join(opts.BuildDir, ids.MemBuildID.String()),
		BuildID: ids.MemBuildID, ParentDir: opts.MemParentDir,
	})
	if err != nil {
		fail(err)
		return
	}
	// What this checkpoint actually added to storage, which is the O(dirty)
	// number. See metrics.SnapshotStoredBytes for why mem.bin's own size --
	// apparent or allocated -- is not it.
	metrics.SnapshotStoredBytes.Observe(float64(memStats.PackedBytes))
	if m.MemMiB > 0 {
		metrics.SnapshotStoredRatio.Observe(
			float64(memStats.PackedBytes) / float64(int64(m.MemMiB)<<20))
	}

	// Deliberately NOT posix_fadvise(DONTNEED) on the image here. Dropping it
	// looks obviously right -- half a gigabyte, read once, never read again --
	// and it measures worse: p50 500ms against 312ms, with the multi-second
	// outliers back. Firecracker fully overwrites this file on the next
	// checkpoint, and on a copy-on-write filesystem that rewrite goes faster
	// with the extents still cached.
	//
	// Under Diff snapshots that reasoning changes and the conclusion holds
	// harder: Firecracker now rewrites only the dirty extents of this file
	// rather than all of it, so it is cumulative state rather than a scratch
	// buffer, and dropping its cache costs the next merge rather than saving
	// anything.

	if ids.RootfsBuildID != uuid.Nil {
		if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
			In:      filepath.Join(localDir, CowFile),
			OutDir:  filepath.Join(opts.BuildDir, ids.RootfsBuildID.String()),
			BuildID: ids.RootfsBuildID, ParentDir: opts.RootfsTemplateDir, Dirty: dirty,
		}); err != nil {
			fail(err)
			return
		}
	}

	// Chunkified is not durable, and the difference matters: a rollback on
	// THIS host needs only the local builds, while a restore anywhere else
	// needs the upload. Collapsing the two would make every rollback wait for
	// an upload it does not use.
	if err := writeBuildIDs(localDir, ids.MemBuildID, ids.RootfsBuildID); err != nil {
		fail(err)
		return
	}
	if err := os.WriteFile(filepath.Join(localDir, chunkedMarker), nil, 0o644); err != nil {
		fail(err)
		return
	}

	// The staged copies have served their purpose: the builds are the durable
	// form and a restore reads those. Keeping both means every checkpoint
	// costs a full memory image of local disk forever -- which on a machine
	// checkpointed a few times is gigabytes, and the resulting writeback
	// pressure lands on the next checkpoint's pause window, where the user
	// feels it. The vmstate stays: it is kilobytes and a rollback needs it.
	for _, name := range []string{CowFile} {
		if err := os.Remove(filepath.Join(localDir, name)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not discard a staged checkpoint copy",
				"machine", m.ID, "file", name, "err", err)
		}
	}

	if err := uploadBuild(ctx, chunks, opts.BuildDir, ids.MemBuildID); err != nil {
		fail(err)
		return
	}
	if ids.RootfsBuildID != uuid.Nil {
		if err := uploadBuild(ctx, chunks, opts.BuildDir, ids.RootfsBuildID); err != nil {
			fail(err)
			return
		}
	}
	if err := up.PutFile(ctx, snapKey, filepath.Join(localDir, SnapFile)); err != nil {
		fail(err)
		return
	}

	if err := os.WriteFile(filepath.Join(localDir, durableMarker), nil, 0o644); err != nil {
		slog.Error("could not mark checkpoint durable", "machine", m.ID, "err", err)
	}
}

// buildIDsFile records which builds a checkpoint produced.
const buildIDsFile = "builds.json"

func writeBuildIDs(dir string, mem, rootfs uuid.UUID) error {
	line := mem.String() + "\n"
	if rootfs != uuid.Nil {
		line += rootfs.String() + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, buildIDsFile), []byte(line), 0o644); err != nil {
		return fmt.Errorf("fc: write build ids: %w", err)
	}
	return nil
}

// ReadBuildIDs reads back what a checkpoint produced. A zero rootfs id means
// the machine had written nothing to disk.
func ReadBuildIDs(dir string) (mem, rootfs uuid.UUID, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, buildIDsFile))
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("fc: read build ids: %w", err)
	}
	lines := strings.Fields(string(raw))
	if len(lines) == 0 {
		return uuid.Nil, uuid.Nil, fmt.Errorf("fc: build id file in %s is empty", dir)
	}
	if mem, err = uuid.Parse(lines[0]); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("fc: parse memory build id: %w", err)
	}
	if len(lines) > 1 {
		if rootfs, err = uuid.Parse(lines[1]); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("fc: parse rootfs build id: %w", err)
		}
	}
	return mem, rootfs, nil
}

// uploadBuild puts a build's header and data into object storage.
//
// The header goes LAST. A restore fetches the header first and derives
// everything from it, so a header that lands before its data describes bytes
// that are not there yet -- and the failure surfaces as corrupt guest memory
// rather than a missing object.
func uploadBuild(ctx context.Context, up Uploader, buildDir string, id uuid.UUID) error {
	return UploadBuild(ctx, up, buildDir, id)
}

// UploadBuild publishes one content-addressed build.
func UploadBuild(ctx context.Context, up Uploader, buildDir string, id uuid.UUID) error {
	dir := filepath.Join(buildDir, id.String())

	if err := up.PutFile(ctx, id.String()+"/data", filepath.Join(dir, "data")); err != nil {
		return fmt.Errorf("fc: upload build %s data: %w", id, err)
	}
	if err := up.PutFile(ctx, id.String()+"/header", filepath.Join(dir, "header")); err != nil {
		return fmt.Errorf("fc: upload build %s header: %w", id, err)
	}
	return nil
}

// TemplateCapture is the golden template a host builds once.
type TemplateCapture struct {
	MemBuildID uuid.UUID
	// SnapPath is the local vmstate file. It stays inside the jail, so the
	// caller must upload it BEFORE killing the machine.
	SnapPath string
}

// CaptureTemplate freezes a settled machine and chunkifies its memory.
//
// The guest is deliberately NOT resumed: this machine exists only to be
// photographed. Every machine on the host is then created by restoring that
// photograph, which is what makes a create a restore rather than a boot.
func (m *Machine) CaptureTemplate(ctx context.Context, opts SnapshotOpts) (TemplateCapture, error) {
	var out TemplateCapture

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		return out, err
	}
	// The disk is not captured here. The template's disk is the golden rootfs
	// itself, chunked directly -- a machine that has only just booted has
	// written nothing worth keeping, and the two must be the same bytes or
	// every restore starts from a disk its memory image does not describe.
	snapshot, err := m.Chunkify(ctx, SnapshotOpts{
		BuildDir: opts.BuildDir, MemParentDir: "",
	})
	if err != nil {
		return out, err
	}

	out.MemBuildID = snapshot.MemBuildID
	out.SnapPath = p.hostSnap
	return out, nil
}

// makeMemoryResident asks the fault handler to install every page.
//
// Best effort. A machine whose handler cannot be reached still snapshots
// correctly -- it just pays for its faults inside the pause instead of before
// it, which is slow rather than wrong.
func (m *Machine) makeMemoryResident() {
	if m.Uffd == nil {
		return
	}
	start := time.Now()
	if err := m.Uffd.MakeResident(); err != nil {
		slog.Warn("could not make memory resident before snapshotting; the "+
			"guest will be paused for its page faults", "machine", m.ID, "err", err)
		return
	}
	slog.Debug("memory made resident before snapshot",
		"machine", m.ID, "ms", time.Since(start).Milliseconds())
}

// A snapshot's expensive half -- reading the memory image, chunkifying it,
// uploading it -- runs in the background so the guest can resume. The next
// snapshot has to wait for it, because the two compete and the second one's
// share of that competition lands inside its pause window.
//
// The wait belongs BEFORE the pause, where the guest is still running and
// serving, rather than inside it.

// awaitCapture blocks until any background capture has finished.
func (m *Machine) awaitCapture() {
	m.captureMu.Lock()
	done := m.captureDone
	m.captureMu.Unlock()

	if done == nil {
		return
	}
	start := time.Now()
	<-done
	slog.Debug("waited for the previous capture", "machine", m.ID,
		"ms", time.Since(start).Milliseconds())
}

// beginCapture marks a background capture as in flight.
func (m *Machine) beginCapture() {
	m.captureMu.Lock()
	defer m.captureMu.Unlock()
	m.captureDone = make(chan struct{})
}

// endCapture releases whoever is waiting on the capture.
func (m *Machine) endCapture() {
	m.captureMu.Lock()
	defer m.captureMu.Unlock()

	if m.captureDone != nil {
		close(m.captureDone)
		m.captureDone = nil
	}
}
