package fc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
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

// Snapshot types, as Firecracker names them on /snapshot/create.
const (
	SnapshotFull = "Full"
	SnapshotDiff = "Diff"
)

// snapshotType decides between writing a whole memory image and merging a
// diff into the one already on disk.
//
// Firecracker merges a Diff INTO mem_file_path when that file exists and is
// exactly mem_size_mib big, and OVERWRITES it with a hole-riddled partial
// image when it is not. The check below is Firecracker's own condition, asked
// first -- because when it does not hold, a Diff does not fail. It silently
// produces an image whose untouched pages read back as zeros, and the machine
// loses its memory on the NEXT restore rather than here.
//
// So the first snapshot of every machine lifetime is Full. After a wake there
// is no local image (SuspendInstant removes it), and after a restore
// Firecracker has reset its own dirty-page accounting.
//
// The file's existence and size are the authority rather than a flag carried
// on the machine, because that is exactly Firecracker's condition, and a flag
// would outlive a mem.bin that some other path removed.
func (m *Machine) snapshotType(hostMem string, memMiB int) string {
	if memMiB <= 0 {
		return SnapshotFull // unknown size: cannot verify the merge condition
	}
	info, err := os.Stat(hostMem)
	if err != nil || info.Size() != int64(memMiB)<<20 {
		return SnapshotFull
	}
	return SnapshotDiff
}

// pauseAndSnapshot freezes the guest and writes its state to disk.
//
// The guest is stopped for exactly this window, so everything that can happen
// afterwards -- uploading, copying -- happens after the resume.
// WhilePaused freezes the guest, runs fn, and resumes it.
//
// For work that has to see a filesystem nobody is writing to: a volume clone,
// above all. A clone taken while the guest is writing captures ext4 mid-update
// -- it mounts, and then fails somewhere later, on a fork nobody will connect
// back to this moment.
//
// The resume is deferred, so it runs whether fn succeeded, failed or panicked.
// A guest left paused is a machine that answers nothing and looks alive, which
// is worse than either outcome of fn. A resume that itself fails is returned
// alongside fn's error rather than instead of it: both are real, and the
// paused guest is the more urgent of the two.
func (m *Machine) WhilePaused(ctx context.Context, fn func() error) (err error) {
	if perr := m.Client.Pause(ctx); perr != nil {
		return fmt.Errorf("fc: pause %s: %w", m.ID, perr)
	}
	defer func() {
		if rerr := m.Client.Resume(ctx); rerr != nil {
			err = errors.Join(err, fmt.Errorf("fc: resume %s after a paused operation: %w", m.ID, rerr))
		}
	}()
	return fn()
}

func (m *Machine) pauseAndSnapshot(ctx context.Context) (snapshotPaths, error) {
	p := m.snapshotPaths()
	kind := m.snapshotType(p.hostMem, m.MemMiB)

	if err := m.Client.Pause(ctx); err != nil {
		return p, fmt.Errorf("fc: pause %s: %w", m.ID, err)
	}
	started := time.Now()
	if err := m.Client.CreateSnapshot(ctx, SnapshotCreate{
		SnapshotType: kind, SnapshotPath: p.jailSnap, MemFilePath: p.jailMem,
	}); err != nil {
		return p, fmt.Errorf("fc: snapshot %s (%s): %w", m.ID, kind, err)
	}
	// Inside the pause, so this is guest-visible freeze time and the number
	// the Full-to-Diff switch moves.
	metrics.SnapshotWriteSeconds.With(kind).Observe(time.Since(started).Seconds())
	m.lastSnapshotType = kind
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

// ErrGuestGone reports that the Firecracker process is no longer running. The
// exit watcher delivers the exit itself; this only stops a caller retrying.
var ErrGuestGone = errors.New("fc: the firecracker process is gone")

// resumeAfterFailure puts a guest back to running after a snapshot attempt
// failed partway, and reports whether the process turned out to be gone.
//
// Gone is a connection-level failure on the API socket (nobody listening, or
// no socket at all) AND a pid that is not running. A live process whose socket
// refuses is left alone: its guest may still be serving, and killing on a
// guess is worse than the frozen state this logs. The machine manager hears
// about a gone process from the exit watcher, not from here.
func (m *Machine) resumeAfterFailure(ctx context.Context, cause error) (gone bool) {
	rerr := m.Client.Resume(context.WithoutCancel(ctx))
	if rerr == nil {
		return false
	}
	if isSocketGone(rerr) && !m.processRunning() {
		slog.Error("snapshot failed because the firecracker process is gone; "+
			"its exit is handled by the exit watcher",
			"machine", m.ID, "cause", cause, "err", rerr)
		return true
	}
	slog.Error("snapshot failed and the guest could not be resumed; it is "+
		"frozen and will not answer",
		"machine", m.ID, "cause", cause, "err", rerr)
	return false
}

// isSocketGone reports a dial that found nobody listening, or no socket at all.
//
// Client.do wraps the transport error with %w, and the dial error chain is
// *url.Error, *net.OpError, *os.SyscallError, syscall.Errno, so errors.Is
// reaches the errno.
func isSocketGone(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// processRunning is processAlive on this machine's pid, false with no pid.
func (m *Machine) processRunning() bool {
	if m.Cmd == nil || m.Cmd.Process == nil {
		return false
	}
	return processAlive(m.Cmd.Process.Pid)
}
