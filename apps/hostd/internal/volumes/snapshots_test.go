package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A snapshot is a CLONE, not a copy. That is what makes it milliseconds
// whatever the volume holds, and it is what stops an overwrite in the live
// image from freeing blocks the snapshot still points at: the clone bumps the
// refcount on every slice it references, inside the same metadata database the
// mount already serialises on.
//
// The alternative -- a point-in-time copy of the metadata database -- names
// slices the live volume has since freed, and making it work needs a trash
// retention or a second refcount. That is a second copy of JuiceFS's own
// bookkeeping.
func TestASnapshotIsAClonePreservingTheFile(t *testing.T) {
	m, rec := newTestManager(t)

	if err := m.Snapshot(context.Background(), "vol-1", "20260912T101500Z"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := rec.find(t, "juicefs clone")
	if !hasFlag(got.args, "--preserve") {
		t.Errorf("clone is not --preserve, so a restored image changes owner: %v", got.args)
	}
	src, dst := got.args[len(got.args)-2], got.args[len(got.args)-1]
	if src != m.ImagePath("vol-1") {
		t.Errorf("cloned from %q, want the live image %q", src, m.ImagePath("vol-1"))
	}
	want := filepath.Join(m.MountPoint("vol-1"), "snapshots", "20260912T101500Z", ImageName)
	if dst != want {
		t.Errorf("cloned to %q, want %q", dst, want)
	}
	// INSIDE the volume's own filesystem. A clone written anywhere else shares
	// no blocks with the image it came from and is a full copy wearing the
	// name of a snapshot.
	if !strings.HasPrefix(dst, m.MountPoint("vol-1")+"/") {
		t.Errorf("the snapshot is outside the volume's filesystem: %q", dst)
	}
}

// The stamp sorts lexically in time order, which is what lets a listing be a
// directory read with no parsing and no stored index.
func TestSnapshotStampsSortInTimeOrder(t *testing.T) {
	earlier := SnapshotStamp(time.Date(2026, 9, 12, 10, 15, 0, 0, time.UTC))
	later := SnapshotStamp(time.Date(2026, 9, 12, 10, 15, 1, 0, time.UTC))
	if !(earlier < later) {
		t.Errorf("%q does not sort before %q", earlier, later)
	}
	// UTC whatever the host's clock is set to: a host's local time is not a
	// fact about the volume.
	if !strings.HasSuffix(earlier, "Z") {
		t.Errorf("%q is not stamped in UTC", earlier)
	}
}

// A volume nobody has snapshotted answers with nothing, not an error. That is
// most volumes, and the list route must not fail on them.
func TestListingAVolumeWithNoSnapshotsIsEmpty(t *testing.T) {
	m, _ := newTestManager(t)

	got, err := m.ListSnapshots("vol-never-snapshotted")
	if err != nil {
		t.Fatalf("listing a volume with no snapshots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

// Newest first, so a caller reaching for "the last one" takes the head of the
// list rather than sorting it again.
func TestSnapshotsAreListedNewestFirst(t *testing.T) {
	m, _ := newTestManager(t)
	root := filepath.Join(m.MountPoint("vol-1"), SnapshotDir)
	for _, stamp := range []string{"20260912T100000Z", "20260912T120000Z", "20260912T110000Z"} {
		if err := os.MkdirAll(filepath.Join(root, stamp), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.ListSnapshots("vol-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"20260912T120000Z", "20260912T110000Z", "20260912T100000Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A restore is a clone to a staging path and a RENAME. The rename is what
// makes it atomic: there is no moment at which the image is half of one
// snapshot and half of another.
func TestRestoreStagesThenRenamesAtomically(t *testing.T) {
	m, rec := newTestManager(t)
	stamp := "20260912T101500Z"

	// The clone is faked by the recorder, so stage the destination by hand:
	// what is under test is the sequence, not juicefs.
	snap := m.snapshotPath("vol-1", stamp)
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snap, []byte("snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := m.ImagePath("vol-1") + ".restore"
	m.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := rec.run(ctx, name, args...)
		// Stand in for what the clone would have produced.
		_ = os.WriteFile(staged, []byte("snapshot"), 0o644)
		return out, err
	}

	if err := m.RestoreSnapshot(context.Background(), "vol-1", stamp); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	got := rec.find(t, "juicefs clone")
	if got.args[len(got.args)-1] != staged {
		t.Errorf("the restore cloned straight over the live image: %v", got.args)
	}
	if body, err := os.ReadFile(m.ImagePath("vol-1")); err != nil || string(body) != "snapshot" {
		t.Errorf("the live image is %q (%v), want the snapshot's contents", body, err)
	}
	// The staging file must not survive: a leftover would make the NEXT
	// restore fail on a path that already exists.
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("the staging file survived the restore")
	}
}

// Restoring a snapshot that does not exist fails before anything is cloned. A
// clone from a missing path would leave the volume untouched anyway, but the
// error a caller sees should name the snapshot rather than juicefs.
func TestRestoringAMissingSnapshotFailsBeforeAnythingRuns(t *testing.T) {
	m, rec := newTestManager(t)

	err := m.RestoreSnapshot(context.Background(), "vol-1", "20260101T000000Z")
	if err == nil {
		t.Fatal("restoring a snapshot that does not exist succeeded")
	}
	if len(rec.calls) != 0 {
		t.Errorf("it ran %v before noticing", rec.names())
	}
}

// A fork copies BYTES, across two filesystems, and says so. JuiceFS slice ids
// and chunk keys are per filesystem, so there is no sharing to be had; the
// alternative would pin every fork to the source volume's host.
func TestForkingCopiesSparsely(t *testing.T) {
	m, rec := newTestManager(t)
	stamp := "20260912T101500Z"
	snap := m.snapshotPath("vol-1", stamp)
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snap, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.CopySnapshotTo(context.Background(), "vol-1", stamp, m.ImagePath("vol-2")); err != nil {
		t.Fatalf("CopySnapshotTo: %v", err)
	}
	got := rec.find(t, "cp --sparse=always")
	if got.args[len(got.args)-2] != snap {
		t.Errorf("copied from %v, want the snapshot", got.args)
	}
	if got.args[len(got.args)-1] != m.ImagePath("vol-2") {
		t.Errorf("copied to %v, want the new volume's image", got.args)
	}
}

// The check forces itself on a filesystem that claims to be clean, and that is
// the whole value: a host that died mid-write leaves an image that still SAYS
// it is clean, and mounting it lets the guest write on top of the damage.
func TestTheFilesystemCheckForcesItselfAndAnswersItsOwnPrompts(t *testing.T) {
	args := fsckArgs("/mnt/vol-1/disk.img")
	if !hasFlag(args, "-f") {
		t.Errorf("the check is not forced, so a dirty filesystem marked clean passes: %v", args)
	}
	if !hasFlag(args, "-y") {
		t.Errorf("the check would stop at a prompt nobody is there to answer: %v", args)
	}
	if args[len(args)-1] != "/mnt/vol-1/disk.img" {
		t.Errorf("the image is not the last argument: %v", args)
	}
}

// Exit 1 is "errors found and corrected", which is what -y asked for and a
// pass. Anything above means the check could not finish, and the volume must
// not be handed to a guest while a snapshot can still be restored.
func TestCheckPassesACorrectedFilesystemAndRefusesAWorseOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exit       int
		wantRefuse bool
	}{
		{"clean", 0, false},
		{"errors corrected", 1, false},
		{"errors left uncorrected", 4, true},
		{"operational error", 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.run = func(context.Context, string, ...string) ([]byte, error) {
				if tc.exit == 0 {
					return nil, nil
				}
				return []byte("fsck output"), fmt.Errorf("e2fsck: %w", exitStatus(tc.exit))
			}
			err := m.Check(context.Background(), "vol-1")
			if tc.wantRefuse && !errors.Is(err, ErrVolumeCorrupt) {
				t.Errorf("exit %d gave %v, want ErrVolumeCorrupt", tc.exit, err)
			}
			if !tc.wantRefuse && err != nil {
				t.Errorf("exit %d was refused: %v", tc.exit, err)
			}
		})
	}
}

// exitStatus stands in for a process that exited with a code, through the same
// errors.As the real runner's wrapping goes through.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }
