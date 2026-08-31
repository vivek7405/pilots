package fc

import (
	"context"
	"fmt"
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

// Uploader is the object-storage surface the snapshot paths need.
type Uploader interface {
	PutFile(ctx context.Context, key, filePath string) error
	GetToFile(ctx context.Context, key, filePath string) error
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
func (m *Machine) Suspend(ctx context.Context, up Uploader, at Artifacts) (SuspendResult, error) {
	res := SuspendResult{Artifacts: at}

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		return res, err
	}
	if err := syncRootfs(p.hostRootfs); err != nil {
		return res, err
	}

	if err := up.PutFile(ctx, at.Snap(), p.hostSnap); err != nil {
		return res, err
	}
	if err := up.PutFile(ctx, at.Mem(), p.hostMem); err != nil {
		return res, err
	}

	allocated, err := hasAllocatedBlocks(p.hostRootfs)
	if err != nil {
		return res, err
	}
	if allocated {
		if err := up.PutFile(ctx, at.Rootfs(), p.hostRootfs); err != nil {
			return res, err
		}
		res.RootfsSaved = true
	}

	// Free the memory image before killing the VM: it is already durable, and
	// on a busy host these are the largest files around.
	_ = os.Remove(p.hostMem)

	return res, m.Kill()
}
