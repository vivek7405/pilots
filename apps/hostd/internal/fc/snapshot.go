package fc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
)

// Snapshot artifact names, both on disk and as object-storage keys.
const (
	SnapFile = "snap.bin" // Firecracker VM state
	MemFile  = "mem.bin"  // guest memory image
)

// snapshotPaths are the in-chroot locations Firecracker writes to, and the
// host-side paths hostd reads them back from.
//
// Firecracker is chrooted, so it must be given paths relative to the jail,
// while hostd needs the same files at their real location.
type snapshotPaths struct {
	jailSnap, jailMem string // as Firecracker sees them
	hostSnap, hostMem string // as hostd sees them
}

func (m *Machine) snapshotPaths() snapshotPaths {
	return snapshotPaths{
		jailSnap: "/" + SnapFile,
		jailMem:  "/" + MemFile,
		hostSnap: filepath.Join(m.ChrootDir, SnapFile),
		hostMem:  filepath.Join(m.ChrootDir, MemFile),
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
