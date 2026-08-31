package fc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Snapshot artifact names, both on disk and as object-storage keys.
const (
	SnapFile   = "snap.bin"    // Firecracker VM state
	MemFile    = "mem.bin"     // guest memory image
	RootfsFile = "rootfs.ext4" // the machine's disk
)

// Artifacts names the objects that make up one restorable point.
//
// Everything needed to reconstruct a machine is here, and all of it lives in
// object storage -- which is what lets any host restore any machine.
type Artifacts struct {
	Prefix string // e.g. "machines/<id>/checkpoints/<ckpt>"

	// Immutable marks a set whose objects are written once and never
	// rewritten, so a local copy is always current. A checkpoint is immutable;
	// a machine's suspend image is not -- it is overwritten on every suspend.
	Immutable bool
}

func (a Artifacts) Snap() string   { return filepath.Join(a.Prefix, SnapFile) }
func (a Artifacts) Mem() string    { return filepath.Join(a.Prefix, MemFile) }
func (a Artifacts) Rootfs() string { return filepath.Join(a.Prefix, RootfsFile) }

// snapshotPaths are the in-chroot locations Firecracker writes to, and the
// host-side paths hostd reads them back from.
//
// Firecracker is chrooted, so it must be given paths relative to the jail,
// while hostd needs the same files at their real location.
type snapshotPaths struct {
	jailSnap, jailMem string // as Firecracker sees them
	hostSnap, hostMem string // as hostd sees them
	hostRootfs        string
}

func (m *Machine) snapshotPaths() snapshotPaths {
	return snapshotPaths{
		jailSnap: "/" + SnapFile,
		jailMem:  "/" + MemFile,
		hostSnap: filepath.Join(m.ChrootDir, SnapFile),
		hostMem:  filepath.Join(m.ChrootDir, MemFile),
		// The rootfs lives at the constant baked path inside the jail.
		hostRootfs: filepath.Join(m.ChrootDir, BakedRootfsPath),
	}
}

// pauseAndSnapshot freezes the guest and writes its state to disk.
//
// The guest is stopped for exactly this window, so everything that can happen
// afterwards -- uploading, copying -- happens after the resume.
func (m *Machine) pauseAndSnapshot(ctx context.Context) (snapshotPaths, error) {
	p := m.snapshotPaths()

	if err := m.Client.Pause(ctx); err != nil {
		return p, fmt.Errorf("fc: pause %s: %w", m.ID, err)
	}
	if err := m.Client.CreateSnapshot(ctx, SnapshotCreate{
		SnapshotType: "Full", SnapshotPath: p.jailSnap, MemFilePath: p.jailMem,
	}); err != nil {
		return p, fmt.Errorf("fc: snapshot %s: %w", m.ID, err)
	}
	return p, nil
}

// syncRootfs flushes the machine's disk.
//
// SyncFileRange on this one file, never sync(2). A global sync takes the
// kernel's block-device lock and flushes every dirty page on the host, so
// concurrent suspends serialise behind each other -- the predecessor measured
// individual calls hanging for over five minutes under load.
func syncRootfs(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("fc: open rootfs for sync: %w", err)
	}
	defer f.Close()

	if err := unix.SyncFileRange(int(f.Fd()), 0, 0,
		unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER,
	); err != nil {
		return fmt.Errorf("fc: sync_file_range: %w", err)
	}
	return nil
}

// hasAllocatedBlocks reports whether a file occupies any disk at all.
//
// A machine that never wrote to its disk produces a rootfs with zero allocated
// blocks. Uploading it would move nothing but cost a full-size transfer, so
// the caller skips it and the restore falls back to the template.
func hasAllocatedBlocks(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false, fmt.Errorf("fc: stat %s: %w", path, err)
	}
	return st.Blocks > 0, nil
}

// ErrArtifactMissing reports an object that is not in the store.
//
// Restore distinguishes it from a failed fetch: a machine that never wrote to
// disk legitimately has no rootfs object and falls back to the template, while
// a network failure must not silently do the same.
var ErrArtifactMissing = errors.New("fc: artifact missing")

// Uploader is the object-storage surface the snapshot paths need.
//
// Implementations MUST return an error satisfying errors.Is(err,
// ErrArtifactMissing) when an object does not exist.
type Uploader interface {
	PutFile(ctx context.Context, key, filePath string) error
	GetToFile(ctx context.Context, key, filePath string) error
}

// UnconfiguredStore stands in when no object storage is configured. Every
// operation fails with an explanation rather than a nil dereference.
type UnconfiguredStore struct{}

func (UnconfiguredStore) PutFile(context.Context, string, string) error {
	return errors.New("fc: no object storage is configured")
}

func (UnconfiguredStore) GetToFile(context.Context, string, string) error {
	return errors.New("fc: no object storage is configured")
}

// SuspendResult reports what a suspend produced.
type SuspendResult struct {
	Artifacts   Artifacts
	RootfsSaved bool // false when the machine never wrote to disk
}

// Suspend snapshots a machine, uploads it, and stops it.
//
// This is the scale-to-zero path: a suspended machine costs nothing but
// storage, and wakes on the next request. The guest is NOT resumed -- the
// caller is deliberately giving up the memory.
func (m *Machine) Suspend(ctx context.Context, up Uploader, at Artifacts) (res SuspendResult, err error) {
	res = SuspendResult{Artifacts: at}

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		// pauseAndSnapshot may have paused before failing.
		m.resumeAfterFailure(ctx, err)
		return res, err
	}

	// Any failure from here until the machine is killed leaves the guest
	// frozen. A paused VM whose row still says "running" is worse than a
	// failed suspend: the router proxies into a machine that can never answer,
	// and the idle monitor retries the same failing suspend every tick.
	defer func() {
		if err != nil {
			m.resumeAfterFailure(ctx, err)
		}
	}()

	if err = syncRootfs(p.hostRootfs); err != nil {
		return res, err
	}

	if err = up.PutFile(ctx, at.Snap(), p.hostSnap); err != nil {
		return res, err
	}
	if err = up.PutFile(ctx, at.Mem(), p.hostMem); err != nil {
		return res, err
	}

	allocated, aerr := hasAllocatedBlocks(p.hostRootfs)
	if aerr != nil {
		err = aerr
		return res, err
	}
	if allocated {
		if err = up.PutFile(ctx, at.Rootfs(), p.hostRootfs); err != nil {
			return res, err
		}
		res.RootfsSaved = true
	}

	// Free the memory image before killing the VM: it is already durable, and
	// on a busy host these are the largest files around.
	_ = os.Remove(p.hostMem)

	err = m.Kill()
	return res, err
}

// resumeAfterFailure puts a guest back to running after a snapshot attempt
// failed partway. Best effort: if even this fails there is nothing left to try
// but say so loudly, because the machine is now frozen and unreachable.
func (m *Machine) resumeAfterFailure(ctx context.Context, cause error) {
	if rerr := m.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
		slog.Error("snapshot failed and the guest could not be resumed; it is "+
			"frozen and will not answer",
			"machine", m.ID, "cause", cause, "err", rerr)
	}
}
