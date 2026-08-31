package fc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// recordingUploader remembers the order keys were written in.
type recordingUploader struct {
	keys   []string
	failOn string
}

func (u *recordingUploader) PutFile(_ context.Context, key, path string) error {
	if key == u.failOn {
		return errors.New("recordingUploader: injected failure")
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	u.keys = append(u.keys, key)
	return nil
}

func (u *recordingUploader) GetToFile(context.Context, string, string) error {
	return errors.New("recordingUploader: not implemented")
}

// stageBuild writes a build directory's two files.
func stageBuild(t *testing.T, root string, id uuid.UUID) {
	t.Helper()
	dir := filepath.Join(root, id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"header", "data"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The header is what a restore reads first and derives everything else from,
// so a header that lands before its data names bytes that are not there yet.
// The failure surfaces as corrupt guest memory, not as a missing object.
func TestUploadBuildPutsTheHeaderLast(t *testing.T) {
	root := t.TempDir()
	id := uuid.New()
	stageBuild(t, root, id)

	up := &recordingUploader{}
	if err := uploadBuild(context.Background(), up, root, id); err != nil {
		t.Fatalf("uploadBuild: %v", err)
	}

	want := []string{id.String() + "/data", id.String() + "/header"}
	if len(up.keys) != 2 || up.keys[0] != want[0] || up.keys[1] != want[1] {
		t.Errorf("uploaded %v, want %v", up.keys, want)
	}
}

// A failed data upload must not leave a header pointing at it.
func TestUploadBuildDoesNotPublishAHeaderWithoutItsData(t *testing.T) {
	root := t.TempDir()
	id := uuid.New()
	stageBuild(t, root, id)

	up := &recordingUploader{failOn: id.String() + "/data"}
	if err := uploadBuild(context.Background(), up, root, id); err == nil {
		t.Fatal("uploadBuild reported success after the data upload failed")
	}
	for _, k := range up.keys {
		if k == id.String()+"/header" {
			t.Error("the header was published even though its data upload failed")
		}
	}
}

func TestBuildIDsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mem, rootfs := uuid.New(), uuid.New()

	if err := writeBuildIDs(dir, mem, rootfs); err != nil {
		t.Fatalf("writeBuildIDs: %v", err)
	}
	gotMem, gotRootfs, err := ReadBuildIDs(dir)
	if err != nil {
		t.Fatalf("ReadBuildIDs: %v", err)
	}
	if gotMem != mem || gotRootfs != rootfs {
		t.Errorf("read %s/%s, wrote %s/%s", gotMem, gotRootfs, mem, rootfs)
	}
}

// A machine that never wrote to disk produces no rootfs build. Reading back a
// non-nil id for it would make the next restore fetch a build that does not
// exist.
func TestBuildIDsRecordAnAbsentRootfs(t *testing.T) {
	dir := t.TempDir()
	mem := uuid.New()

	if err := writeBuildIDs(dir, mem, uuid.Nil); err != nil {
		t.Fatalf("writeBuildIDs: %v", err)
	}
	gotMem, gotRootfs, err := ReadBuildIDs(dir)
	if err != nil {
		t.Fatalf("ReadBuildIDs: %v", err)
	}
	if gotMem != mem {
		t.Errorf("memory build id = %s, want %s", gotMem, mem)
	}
	if gotRootfs != uuid.Nil {
		t.Errorf("rootfs build id = %s, want the zero uuid", gotRootfs)
	}
}

// Every function must run even when an earlier one fails: each may have
// started a process or a namespace, and short-circuiting would leave those
// unaccounted for with nothing holding a handle to clean them up.
func TestInParallelRunsEveryFunctionAndJoinsTheirErrors(t *testing.T) {
	var ran atomic.Int32
	first := errors.New("first")
	third := errors.New("third")

	err := inParallel(
		func() error { ran.Add(1); return first },
		func() error { ran.Add(1); return nil },
		func() error { ran.Add(1); return third },
	)

	if ran.Load() != 3 {
		t.Errorf("%d of 3 functions ran", ran.Load())
	}
	if !errors.Is(err, first) || !errors.Is(err, third) {
		t.Errorf("joined error %v does not carry both failures", err)
	}
}

func TestInParallelReportsSuccess(t *testing.T) {
	if err := inParallel(func() error { return nil }, func() error { return nil }); err != nil {
		t.Errorf("inParallel: %v", err)
	}
}

// Firecracker is chrooted and cannot reach /dev/nbdN, so the device has to
// exist under a second name inside the jail. Same major and minor, or it is a
// different device -- which means the guest reads someone else's disk.
func TestMknodBlockDeviceReproducesTheDevice(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: mknod")
	}
	const source = "/dev/nbd0"
	var src unix.Stat_t
	if err := unix.Stat(source, &src); err != nil {
		t.Skipf("no %s: modprobe nbd nbds_max=64", source)
	}

	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := mknodBlockDevice(source, dest, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("mknodBlockDevice: %v", err)
	}

	var dst unix.Stat_t
	if err := unix.Stat(dest, &dst); err != nil {
		t.Fatalf("stat %s: %v", dest, err)
	}
	if dst.Rdev != src.Rdev {
		t.Errorf("device is %d:%d, want %d:%d",
			unix.Major(dst.Rdev), unix.Minor(dst.Rdev),
			unix.Major(src.Rdev), unix.Minor(src.Rdev))
	}
	if dst.Mode&unix.S_IFMT != unix.S_IFBLK {
		t.Errorf("mode %#o is not a block device", dst.Mode)
	}
}

// A stale node from a previous lifetime points at a device that now belongs to
// another machine. Leaving it in place hands one machine another's disk.
func TestMknodBlockDeviceReplacesAStaleNode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: mknod")
	}
	var src unix.Stat_t
	if err := unix.Stat("/dev/nbd0", &src); err != nil {
		t.Skip("no /dev/nbd0")
	}

	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(dest, []byte("a regular file from a previous life"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mknodBlockDevice("/dev/nbd0", dest, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("mknodBlockDevice over an existing path: %v", err)
	}
	var dst unix.Stat_t
	if err := unix.Stat(dest, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Mode&unix.S_IFMT != unix.S_IFBLK || dst.Rdev != src.Rdev {
		t.Error("the stale path was not replaced by the device node")
	}
}

// A machine with no block server -- one booted from a template file rather
// than restored -- must still chunkify its memory rather than failing.
func TestChunkifyWithoutABlockServerProducesMemoryOnly(t *testing.T) {
	dir := t.TempDir()

	m := &Machine{ID: "m-1", ChrootDir: dir, StateDir: dir}
	if err := os.WriteFile(filepath.Join(dir, MemFile), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := m.Chunkify(context.Background(), SnapshotOpts{
		BuildDir: filepath.Join(dir, "builds"),
	})
	if err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	if got.MemBuildID == uuid.Nil {
		t.Error("no memory build was produced")
	}
	if got.RootfsBuildID != uuid.Nil {
		t.Errorf("a rootfs build was produced with no block server: %s", got.RootfsBuildID)
	}
}

// stopHandlers must be safe on a machine that has none, and must clear them so
// a second teardown -- a retried destroy, or the reaper racing an explicit one
// -- does not signal a pid that has since been recycled.
func TestStopHandlersIsSafeAndIdempotent(t *testing.T) {
	m := &Machine{ID: "m-1"}
	if errs := m.stopHandlers(); len(errs) != 0 {
		t.Errorf("stopHandlers on a machine with none returned %v", errs)
	}
	if errs := m.stopHandlers(); len(errs) != 0 {
		t.Errorf("second call returned %v", errs)
	}
}
