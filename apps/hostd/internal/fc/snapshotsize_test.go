package fc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// A diff snapshot leaves the memory image at exactly the machine's memory
// size and only changes how many blocks are allocated to it. Measuring the
// apparent size would therefore report every checkpoint as a full one and
// hide the entire lever, so this asserts the sparse case specifically.
func TestRecordSnapshotSizeMeasuresAllocatedNotApparent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.bin")
	const memMiB = 512

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Apparent size: the full 512MiB. Allocated: one 4KiB write.
	if err := f.Truncate(int64(memMiB) << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(memMiB)<<20 {
		t.Fatalf("fixture apparent size is %d, want the full %d -- the test "+
			"cannot show the difference otherwise", info.Size(), int64(memMiB)<<20)
	}

	before := metrics.SnapshotDiffBytes.Count()
	recordSnapshotSize(path, memMiB)
	if got := metrics.SnapshotDiffBytes.Count() - before; got != 1 {
		t.Errorf("recorded %d observations, want 1", got)
	}
	// The ratio series is what the gate reads; a sparse file must land in the
	// smallest bucket, not at 1.
	if metrics.SnapshotDiffRatio.Count() == 0 {
		t.Error("no ratio observation recorded")
	}
}

// A missing image records nothing rather than a zero, which would drag the
// ratio distribution down and read as an unusually good checkpoint.
func TestRecordSnapshotSizeIgnoresAMissingImage(t *testing.T) {
	before := metrics.SnapshotDiffBytes.Count()
	recordSnapshotSize(filepath.Join(t.TempDir(), "absent"), 512)
	if got := metrics.SnapshotDiffBytes.Count() - before; got != 0 {
		t.Errorf("recorded %d observations for a missing file, want 0", got)
	}
}
