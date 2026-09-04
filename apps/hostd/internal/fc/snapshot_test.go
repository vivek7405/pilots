package fc

import (
	"os"
	"path/filepath"
	"testing"
)

// writeImage lays down a memory image of exactly n bytes.
func writeImage(t *testing.T, dir string, n int64) string {
	t.Helper()
	path := filepath.Join(dir, MemFile)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
	return path
}

// A machine with no local image on disk has to take a Full. This is the
// post-wake case: SuspendInstant removes the image, so the next snapshot has
// nothing to merge into.
func TestSnapshotTypeIsFullWithoutAnExistingImage(t *testing.T) {
	m := &Machine{MemMiB: 512}
	path := filepath.Join(t.TempDir(), MemFile)

	if got := m.snapshotType(path, 512); got != SnapshotFull {
		t.Errorf("snapshotType = %q with no image, want %q", got, SnapshotFull)
	}
}

// THIS TEST STANDS BETWEEN THE CODEBASE AND SILENT MEMORY LOSS.
//
// It encodes Firecracker's own merge condition: a Diff is merged in place
// only when the target is exactly mem_size_mib. Against a file of any other
// size Firecracker OVERWRITES it with a partial image, whose untouched pages
// read back as zeros -- and the machine loses its memory on the next restore,
// one step removed from the cause.
func TestSnapshotTypeIsFullWhenTheImageIsTheWrongSize(t *testing.T) {
	dir := t.TempDir()
	const memMiB = 512
	path := writeImage(t, dir, int64(memMiB)<<20-1) // one byte short

	m := &Machine{MemMiB: memMiB}
	if got := m.snapshotType(path, memMiB); got != SnapshotFull {
		t.Errorf("snapshotType = %q for an undersized image, want %q -- a Diff "+
			"here silently destroys the machine's memory", got, SnapshotFull)
	}
}

func TestSnapshotTypeIsDiffForAnExactSizedImage(t *testing.T) {
	dir := t.TempDir()
	const memMiB = 512
	path := writeImage(t, dir, int64(memMiB)<<20)

	m := &Machine{MemMiB: memMiB}
	if got := m.snapshotType(path, memMiB); got != SnapshotDiff {
		t.Errorf("snapshotType = %q for an exact-sized image, want %q", got, SnapshotDiff)
	}
}

// A machine adopted from a state file written before MemMiB was persisted has
// no size to verify the merge condition against. Full is the safe direction:
// slow rather than wrong.
func TestSnapshotTypeIsFullWithoutAKnownMemorySize(t *testing.T) {
	dir := t.TempDir()
	path := writeImage(t, dir, 512<<20)

	m := &Machine{}
	if got := m.snapshotType(path, 0); got != SnapshotFull {
		t.Errorf("snapshotType = %q with an unknown memory size, want %q",
			got, SnapshotFull)
	}
}

// An image LARGER than the configured memory is just as wrong as a smaller
// one, and is what a machine resized downward would leave behind.
func TestSnapshotTypeIsFullWhenTheImageIsTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := writeImage(t, dir, 1024<<20)

	m := &Machine{MemMiB: 512}
	if got := m.snapshotType(path, 512); got != SnapshotFull {
		t.Errorf("snapshotType = %q for an oversized image, want %q", got, SnapshotFull)
	}
}
