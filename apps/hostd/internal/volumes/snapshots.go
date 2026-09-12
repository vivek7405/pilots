package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Point-in-time copies of a volume, and forks from them.
//
// # Why `juicefs clone` and not a copy of the metadata database
//
// A volume is one ext4 image inside a JuiceFS filesystem whose metadata lives
// in SQLite, replicated by Litestream. The obvious snapshot is a point-in-time
// copy of that database -- and it is wrong. Such a copy names slices the LIVE
// volume has since overwritten and freed, so restoring it reads blocks that are
// no longer in the bucket. Making it work needs a trash retention or a second
// refcount, which is a second copy of JuiceFS's own bookkeeping: exactly the
// kind of duplicated contract the architecture bar forbids.
//
// `juicefs clone` copies a file's metadata INSIDE the filesystem and bumps the
// refcount on every slice it references. An overwrite in the live image then
// frees nothing a clone still points at. That is the block-reference guard
// `--trash-days 0` needs, for free, in the same database the mount already
// serialises on.
//
// # Why a snapshot is cheap and a fork is not
//
// A snapshot is metadata: no blocks move, so it is milliseconds whatever the
// volume holds. A FORK is a different filesystem -- new id, own format, own
// object-storage prefix -- because JuiceFS slice ids and chunk keys are per
// filesystem. One filesystem holding a family of images would pin every fork to
// the source's host, since a JuiceFS filesystem has one metadata database and
// one mount. So a fork copies bytes, and it is the one operation here that is
// not sub-second. That cost is stated rather than hidden: the gate prints it.

// SnapshotDir is where clones live inside a volume's own filesystem.
//
// Inside the volume on purpose. The clone has to share the filesystem with the
// image it was cloned from, or it shares no blocks with it and is a full copy.
const SnapshotDir = "snapshots"

// ErrVolumeCorrupt says a volume's filesystem failed its check.
//
// A distinct error because the answer to it is distinct: not "retry", but
// "restore a snapshot". Attaching a corrupt image would mount it, let the guest
// write to it, and turn a recoverable filesystem into an unrecoverable one.
var ErrVolumeCorrupt = errors.New("volumes: filesystem check failed")

// SnapshotStamp formats a snapshot's name.
//
// Sortable as a string, so listing a prefix gives them in order with no parsing
// and no stored index. UTC because a host's local time is not a fact about the
// volume.
func SnapshotStamp(at time.Time) string { return at.UTC().Format("20060102T150405Z") }

// snapshotPath is where one snapshot's image lives inside the mount.
func (m *Manager) snapshotPath(id, stamp string) string {
	return filepath.Join(m.MountPoint(id), SnapshotDir, stamp, ImageName)
}

// cloneArgs builds `juicefs clone`.
//
// Exported through a builder rather than inlined for the reason mountArgs is:
// the flags ARE the behaviour, and on a machine with no JuiceFS installed the
// only way to check them is to look at what would have been run.
func (m *Manager) cloneArgs(from, to string) []string {
	// --preserve keeps mode, owner and timestamps, so a restored image is the
	// same file the guest had rather than one root happens to own now.
	return []string{"clone", "--preserve", from, to}
}

// fsckArgs builds the filesystem check run before a volume is attached.
//
// -f forces a check on a filesystem marked clean, which is the whole point: a
// host that died mid-write leaves an image that still SAYS it is clean. -y
// answers every repair prompt, because there is no operator at a prompt inside
// a machine create. -C 0 writes progress to stdout so a long check on a large
// volume is visible in the log rather than looking like a hang.
func fsckArgs(image string) []string {
	return []string{"-f", "-y", "-C", "0", image}
}

// Snapshot takes a point-in-time copy of a volume's image.
//
// The caller must have made the image quiescent first: hostd fsyncs it, and for
// a running machine the clone is taken while the guest is PAUSED. A clone taken
// under a live guest captures a half-written filesystem, which looks fine until
// something tries to mount it.
func (m *Manager) Snapshot(ctx context.Context, id, stamp string) error {
	if stamp == "" {
		stamp = SnapshotStamp(time.Now())
	}
	dir := filepath.Dir(m.snapshotPath(id, stamp))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("volumes: make %s: %w", dir, err)
	}
	if _, err := m.run(ctx, m.cfg.JuiceFSBin,
		m.cloneArgs(m.ImagePath(id), m.snapshotPath(id, stamp))...); err != nil {
		return fmt.Errorf("volumes: snapshot %s at %s: %w", id, stamp, err)
	}
	return nil
}

// ListSnapshots names a volume's snapshots, newest first.
//
// Read from the mount rather than from a replicated table. There is no row to
// keep in step with the filesystem, and no forwarded read: the snapshots ARE
// the directory, so a host that can mount the volume can answer.
func (m *Manager) ListSnapshots(id string) ([]string, error) {
	root := filepath.Join(m.MountPoint(id), SnapshotDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			// A volume nobody has snapshotted, which is most of them.
			return nil, nil
		}
		return nil, fmt.Errorf("volumes: list snapshots of %s: %w", id, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	// Newest first. The stamp sorts lexically in time order, so this needs no
	// parsing and cannot disagree with the names.
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// RestoreSnapshot puts a snapshot back as the volume's live image.
//
// A clone and a rename, both metadata: no blocks move, so this is milliseconds
// however large the volume is. The rename is what makes it atomic -- there is
// no moment at which the image is half of one and half of the other.
//
// The CALLER is responsible for the machine: a volume being written while its
// image is replaced underneath is a corrupted filesystem, and the API refuses
// that case rather than this function guessing at it.
func (m *Manager) RestoreSnapshot(ctx context.Context, id, stamp string) error {
	src := m.snapshotPath(id, stamp)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("volumes: snapshot %s of %s: %w", stamp, id, err)
	}
	staged := m.ImagePath(id) + ".restore"
	// Any leftover from an interrupted restore. Left in place it would make
	// the clone below fail on a path that already exists, and the volume would
	// then be unrestorable until somebody logged in.
	_ = os.Remove(staged)

	if _, err := m.run(ctx, m.cfg.JuiceFSBin, m.cloneArgs(src, staged)...); err != nil {
		return fmt.Errorf("volumes: stage snapshot %s of %s: %w", stamp, id, err)
	}
	if err := os.Rename(staged, m.ImagePath(id)); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("volumes: swap in snapshot %s of %s: %w", stamp, id, err)
	}
	return nil
}

// CopySnapshotTo fills another volume's image from one of this volume's
// snapshots.
//
// Across two filesystems, so this copies BYTES. JuiceFS slice ids and chunk
// keys are per filesystem, so there is no sharing to be had: the alternative --
// one filesystem holding a family of images -- has one metadata database and
// one mount, which would pin every fork to the source's host.
//
// --sparse=always so a volume that is mostly empty, which is most of them,
// copies only what was written.
func (m *Manager) CopySnapshotTo(ctx context.Context, id, stamp, destImage string) error {
	src := m.snapshotPath(id, stamp)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("volumes: snapshot %s of %s: %w", stamp, id, err)
	}
	if _, err := m.run(ctx, "cp", "--sparse=always", src, destImage); err != nil {
		return fmt.Errorf("volumes: copy snapshot %s of %s: %w", stamp, id, err)
	}
	return nil
}

// Check runs a filesystem check on a volume's image.
//
// Called before the image is handed to a guest, and that ordering is the whole
// value: a host that died mid-write leaves an image that still says it is
// clean, and mounting it lets the guest write on top of the damage. e2fsck
// turns a recoverable filesystem into a repaired one here, or refuses the
// attach while a snapshot can still be restored.
//
// Exit 0 is clean and exit 1 is "errors corrected", which is a pass: that is
// what -y asked for. Anything above means the check could not finish the job,
// and the volume is not handed over.
func (m *Manager) Check(ctx context.Context, id string) error {
	out, err := m.run(ctx, "e2fsck", fsckArgs(m.ImagePath(id))...)
	if err == nil {
		return nil
	}
	if code, ok := exitCode(err); ok && code == 1 {
		// Errors found and corrected. Worth saying out loud -- it means this
		// volume's host died mid-write at some point -- but not worth refusing.
		return nil
	}
	return fmt.Errorf("%w: %s: %s", ErrVolumeCorrupt, id, strings.TrimSpace(string(out)))
}

// exitCode pulls a process exit status out of an error, through whatever
// wrapping the runner added.
func exitCode(err error) (int, bool) {
	var ex interface{ ExitCode() int }
	if errors.As(err, &ex) {
		return ex.ExitCode(), true
	}
	return 0, false
}
