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
				},
				Env: cfg.Env, LogFile: logFile,
			})
			return perr
		},
		func() error {
			// The vmstate file is the one artifact still fetched whole. It is
			// kilobytes: device state and vcpu registers, not memory.
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
	opts SnapshotOpts, localDir, snapKey string) (err error) {

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fmt.Errorf("fc: mkdir checkpoint dir: %w", err)
	}
	// This directory may be a retry of a failed attempt.
	_ = os.Remove(filepath.Join(localDir, durableMarker))
	_ = os.Remove(filepath.Join(localDir, failedMarker))

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		if rerr := m.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
			slog.Error("checkpoint failed and the guest could not be resumed",
				"machine", m.ID, "err", rerr)
		}
		return err
	}

	// Resume as soon as the local copies exist. Everything after runs with the
	// machine already serving.
	defer func() {
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
		return err
	}

	localSnap := filepath.Join(localDir, SnapFile)
	localMem := filepath.Join(localDir, MemFile)
	localCow := filepath.Join(localDir, CowFile)

	if err := reflinkCopy(p.hostSnap, localSnap); err != nil {
		return err
	}
	if err := reflinkCopy(p.hostMem, localMem); err != nil {
		return err
	}
	if !dirty.IsEmpty() {
		if err := reflinkCopy(CowPath(m.StateDir), localCow); err != nil {
			return err
		}
	}

	go m.finishCheckpoint(up, chunks, opts, localDir, snapKey, dirty)
	return nil
}

// finishCheckpoint chunkifies and uploads a checkpoint's staged copies.
func (m *Machine) finishCheckpoint(up Uploader, chunks Uploader, opts SnapshotOpts,
	localDir, snapKey string, dirty *roaring.Bitmap) {

	uploadSlots <- struct{}{}
	defer func() { <-uploadSlots }()

	// Detached from any request context: a client that hangs up must not
	// abandon a half-uploaded checkpoint.
	ctx := context.Background()

	fail := func(err error) {
		slog.Error("checkpoint upload failed", "machine", m.ID, "err", err)
		_ = os.WriteFile(filepath.Join(localDir, failedMarker), []byte(err.Error()), 0o644)
	}

	memBuild := uuid.New()
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In:      filepath.Join(localDir, MemFile),
		OutDir:  filepath.Join(opts.BuildDir, memBuild.String()),
		BuildID: memBuild, ParentDir: opts.MemParentDir,
	}); err != nil {
		fail(err)
		return
	}
	if err := uploadBuild(ctx, chunks, opts.BuildDir, memBuild); err != nil {
		fail(err)
		return
	}

	rootfsBuild := uuid.Nil
	if !dirty.IsEmpty() {
		rootfsBuild = uuid.New()
		if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
			In:      filepath.Join(localDir, CowFile),
			OutDir:  filepath.Join(opts.BuildDir, rootfsBuild.String()),
			BuildID: rootfsBuild, ParentDir: opts.RootfsTemplateDir, Dirty: dirty,
		}); err != nil {
			fail(err)
			return
		}
		if err := uploadBuild(ctx, chunks, opts.BuildDir, rootfsBuild); err != nil {
			fail(err)
			return
		}
	}

	if err := up.PutFile(ctx, snapKey, filepath.Join(localDir, SnapFile)); err != nil {
		fail(err)
		return
	}

	// The build ids are recorded next to the markers so the row can be
	// completed by whoever observes durability, without holding them in
	// memory across a restart.
	if err := writeBuildIDs(localDir, memBuild, rootfsBuild); err != nil {
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
	dir := filepath.Join(buildDir, id.String())

	if err := up.PutFile(ctx, id.String()+"/data", filepath.Join(dir, "data")); err != nil {
		return fmt.Errorf("fc: upload build %s data: %w", id, err)
	}
	if err := up.PutFile(ctx, id.String()+"/header", filepath.Join(dir, "header")); err != nil {
		return fmt.Errorf("fc: upload build %s header: %w", id, err)
	}
	return nil
}
