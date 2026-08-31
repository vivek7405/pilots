package block

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func newTestCache(t *testing.T, size int64) *Cache {
	t.Helper()
	c, err := NewCache(size, testBlock, filepath.Join(t.TempDir(), "cow"), false)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A range is served only if EVERY block covering it has been written. Serving
// a partially-owned range would silently mix this cache's zeros with the
// template's real content.
func TestCacheReadRequiresEveryBlockWritten(t *testing.T) {
	c := newTestCache(t, 4*testBlock)

	payload := bytes.Repeat([]byte{7}, int(testBlock))
	if _, err := c.WriteAt(payload, testBlock); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// The written block reads back.
	got := make([]byte, testBlock)
	if _, err := c.ReadAt(got, testBlock); err != nil {
		t.Fatalf("ReadAt on a written block: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("written block did not read back")
	}

	// An unwritten block is a miss, not zeros.
	if _, err := c.ReadAt(make([]byte, testBlock), 0); !errors.Is(err, ErrBytesNotAvailable) {
		t.Errorf("unwritten block: got %v, want ErrBytesNotAvailable", err)
	}

	// A span crossing written and unwritten is a miss too.
	if _, err := c.ReadAt(make([]byte, 2*testBlock), 0); !errors.Is(err, ErrBytesNotAvailable) {
		t.Errorf("partially written span: got %v, want ErrBytesNotAvailable", err)
	}
}

// Written zeros and never-written are identical on disk and mean opposite
// things, which is the entire reason the dirty set exists.
func TestCacheDistinguishesWrittenZerosFromUnwritten(t *testing.T) {
	c := newTestCache(t, 2*testBlock)

	if _, err := c.WriteAt(make([]byte, testBlock), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if _, err := c.ReadAt(make([]byte, testBlock), 0); err != nil {
		t.Errorf("explicitly written zeros were reported unavailable: %v", err)
	}
	if _, err := c.ReadAt(make([]byte, testBlock), testBlock); !errors.Is(err, ErrBytesNotAvailable) {
		t.Error("an untouched block was served as if written")
	}
}

// The file is truncated to full size but allocates nothing until written, so a
// machine that touches a little of its disk costs a little disk.
func TestCacheFileIsSparse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cow")
	c, err := NewCache(64*testBlock, testBlock, path, false)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	defer c.Close()

	if _, err := c.WriteAt(bytes.Repeat([]byte{1}, int(testBlock)), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 64*testBlock {
		t.Errorf("apparent size = %d, want %d", info.Size(), 64*testBlock)
	}
	// st_blocks is in 512-byte units; one written block should be far below
	// the full apparent size.
	if allocated := blocksAllocated(t, path); allocated >= 64*testBlock {
		t.Errorf("allocated %d bytes for one written block; the file is not sparse", allocated)
	}
}

// A zero-size cache is legal -- a machine with no disk of its own -- and must
// not try to map an empty region.
func TestZeroSizeCache(t *testing.T) {
	c, err := NewCache(0, testBlock, filepath.Join(t.TempDir(), "cow"), false)
	if err != nil {
		t.Fatalf("NewCache(0): %v", err)
	}
	defer c.Close()

	if _, err := c.ReadAt(make([]byte, 16), 0); !errors.Is(err, ErrBytesNotAvailable) {
		t.Errorf("got %v, want ErrBytesNotAvailable", err)
	}
}

// Close removes the backing file, which is why the NBD handler must never
// close its cache on exit: the file holds writes that still have to be
// chunkified.
func TestCacheCloseRemovesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cow")
	c, err := NewCache(2*testBlock, testBlock, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file was not created: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Close left the backing file behind")
	}
}

func TestExportToDiffCopiesOnlyWrittenBlocks(t *testing.T) {
	c := newTestCache(t, 4*testBlock)

	first := bytes.Repeat([]byte{1}, int(testBlock))
	third := bytes.Repeat([]byte{3}, int(testBlock))
	if _, err := c.WriteAt(first, testBlock); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteAt(third, 3*testBlock); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "diff")
	dirty, err := c.ExportToDiff(out)
	if err != nil {
		t.Fatalf("ExportToDiff: %v", err)
	}
	if dirty.GetCardinality() != 2 {
		t.Errorf("exported %d dirty blocks, want 2", dirty.GetCardinality())
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[testBlock:2*testBlock], first) {
		t.Error("block 1 did not survive the export")
	}
	if !bytes.Equal(raw[3*testBlock:4*testBlock], third) {
		t.Error("block 3 did not survive the export")
	}
	// Untouched blocks stay zero.
	if !bytes.Equal(raw[0:testBlock], make([]byte, testBlock)) {
		t.Error("an untouched block was written by the export")
	}
}

// Rehydrating a build into a cache is what makes a machine's previous writes
// visible on its first read after a wake.
func TestPopulateFromSlicerRestoresWrites(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	in := writeBlocks(t, dir, 0, 5, 0, 6)
	buildDir := filepath.Join(dir, "build")
	chunkifyTo(t, in, buildDir, "", uuid.New())

	src, err := OpenLocalBuild(buildDir)
	if err != nil {
		t.Fatalf("OpenLocalBuild: %v", err)
	}
	defer src.Close()

	c := newTestCache(t, 4*testBlock)
	if err := c.PopulateFromSlicer(ctx, src); err != nil {
		t.Fatalf("PopulateFromSlicer: %v", err)
	}

	// The stored blocks are now owned by the cache and read back.
	for _, tc := range []struct {
		off  int64
		fill byte
	}{{testBlock, 5}, {3 * testBlock, 6}} {
		got := make([]byte, testBlock)
		if _, err := c.ReadAt(got, tc.off); err != nil {
			t.Fatalf("ReadAt %d after rehydrate: %v", tc.off, err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{tc.fill}, int(testBlock))) {
			t.Errorf("block at %d did not rehydrate", tc.off)
		}
	}

	// Blocks the build elided must stay UNOWNED, so a read falls through to
	// whatever sits underneath rather than being served as this cache's zeros.
	if _, err := c.ReadAt(make([]byte, testBlock), 0); !errors.Is(err, ErrBytesNotAvailable) {
		t.Error("an elided block was marked owned by the rehydrate")
	}
}

// A mapping pointing at ANOTHER build means "unchanged, read it from the
// template at runtime". Marking it owned here would make the cache's zeros
// shadow the template's real content -- silent disk corruption with no error.
func TestPopulateFromSlicerSkipsParentMappings(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	parentIn := writeBlocks(t, dir, 1, 2, 3, 4)
	parentDir := filepath.Join(dir, "parent")
	chunkifyTo(t, parentIn, parentDir, "", uuid.New())

	// Change only block 2; blocks 0, 1 and 3 stay parent-served.
	childPath := filepath.Join(dir, "child.bin")
	var buf bytes.Buffer
	for _, f := range []byte{1, 2, 9, 4} {
		buf.Write(bytes.Repeat([]byte{f}, int(testBlock)))
	}
	if err := os.WriteFile(childPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	childDir := filepath.Join(dir, "child")
	chunkifyTo(t, childPath, childDir, parentDir, uuid.New())

	child, err := OpenLocalBuild(childDir)
	if err != nil {
		t.Fatalf("OpenLocalBuild: %v", err)
	}
	defer child.Close()

	c := newTestCache(t, 4*testBlock)
	if err := c.PopulateFromSlicer(ctx, child); err != nil {
		t.Fatalf("PopulateFromSlicer: %v", err)
	}

	// Only the changed block is owned.
	got := make([]byte, testBlock)
	if _, err := c.ReadAt(got, 2*testBlock); err != nil {
		t.Fatalf("the changed block was not rehydrated: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{9}, int(testBlock))) {
		t.Error("the changed block rehydrated with the wrong content")
	}

	for _, off := range []int64{0, testBlock, 3 * testBlock} {
		if _, err := c.ReadAt(make([]byte, testBlock), off); !errors.Is(err, ErrBytesNotAvailable) {
			t.Errorf("parent-served block at %d was marked owned; the cache's zeros "+
				"would now shadow the template's real content", off)
		}
	}
}

// The overlay is what gives a machine its own disk without copying one.
func TestOverlayFallsThroughToTemplate(t *testing.T) {
	dir := t.TempDir()

	in := writeBlocks(t, dir, 1, 2, 3, 4)
	buildDir := filepath.Join(dir, "template")
	chunkifyTo(t, in, buildDir, "", uuid.New())

	template, err := OpenLocalBuild(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()

	c := newTestCache(t, template.Size())
	o := NewOverlay(template, c)

	// Before any write, everything comes from the template.
	got := make([]byte, testBlock)
	if _, err := o.ReadAt(got, testBlock); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{2}, int(testBlock))) {
		t.Error("read did not fall through to the template")
	}

	// After a write, that block comes from the cache and the rest still does not.
	mine := bytes.Repeat([]byte{99}, int(testBlock))
	if _, err := o.WriteAt(mine, testBlock); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := o.ReadAt(got, testBlock); err != nil {
		t.Fatalf("ReadAt after write: %v", err)
	}
	if !bytes.Equal(got, mine) {
		t.Error("the written block did not come from the cache")
	}
	if _, err := o.ReadAt(got, 2*testBlock); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{3}, int(testBlock))) {
		t.Error("an unwritten block stopped falling through after a neighbour was written")
	}
}

// A read can span written and unwritten blocks, and each half has to come from
// a different place -- which is why the overlay works per block, not per call.
func TestOverlayReadSpanningWrittenAndTemplate(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1, 2, 3, 4)
	buildDir := filepath.Join(dir, "template")
	chunkifyTo(t, in, buildDir, "", uuid.New())

	template, err := OpenLocalBuild(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()

	o := NewOverlay(template, newTestCache(t, template.Size()))
	mine := bytes.Repeat([]byte{99}, int(testBlock))
	if _, err := o.WriteAt(mine, testBlock); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4*testBlock)
	if _, err := o.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	want := bytes.Join([][]byte{
		bytes.Repeat([]byte{1}, int(testBlock)),
		mine,
		bytes.Repeat([]byte{3}, int(testBlock)),
		bytes.Repeat([]byte{4}, int(testBlock)),
	}, nil)
	if !bytes.Equal(got, want) {
		t.Errorf("spanning read differs at byte %d", firstDiff(got, want))
	}
}

// Ejecting hands the cache to the caller and makes Close a no-op, so the file
// survives to be chunkified after the device is torn down.
func TestOverlayEjectCache(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1)
	buildDir := filepath.Join(dir, "template")
	chunkifyTo(t, in, buildDir, "", uuid.New())

	template, err := OpenLocalBuild(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()

	path := filepath.Join(t.TempDir(), "cow")
	c, err := NewCache(template.Size(), testBlock, path, false)
	if err != nil {
		t.Fatal(err)
	}
	o := NewOverlay(template, c)

	if _, err := o.EjectCache(); err != nil {
		t.Fatalf("EjectCache: %v", err)
	}
	if _, err := o.EjectCache(); err == nil {
		t.Error("a second eject succeeded; it must be one-shot")
	}

	if err := o.Close(); err != nil {
		t.Fatalf("Close after eject: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("Close deleted an ejected cache; the machine's un-chunkified writes are gone")
	}
	_ = c.Close()
}

// blocksAllocated reports the bytes a file actually occupies, which for a
// sparse file is far less than its apparent size.
func blocksAllocated(t *testing.T, path string) int64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}
