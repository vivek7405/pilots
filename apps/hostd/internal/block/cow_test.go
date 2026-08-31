package block

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
)

// buildTemplate chunkifies an input as a generation-0 template and returns its
// directory.
func buildTemplate(t *testing.T, dir, in string) string {
	t.Helper()
	out := filepath.Join(dir, "template")
	if _, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: out, BuildID: uuid.New(), BlockSize: testBlock,
	}); err != nil {
		t.Fatalf("Chunkify(template): %v", err)
	}
	return out
}

// A CoW cache is sparse: an untouched block reads back as zeros, which on disk
// is indistinguishable from a block the guest deliberately zeroed. Diffing one
// byte-for-byte records every untouched block as "zeros, mine" and the result
// shadows the template with holes -- a rootfs that mounts and is empty.
//
// This is the single most destructive way to get the CoW path wrong, so it is
// asserted directly: the same input, with and without the bitmap.
func TestCowDiffWithoutTheBitmapWouldEraseTheTemplate(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	templateIn := writeBlocks(t, dir, 1, 2, 3, 4)
	templateDir := buildTemplate(t, dir, templateIn)

	// The cache: only block 1 was ever written. The rest are holes.
	cowPath := filepath.Join(dir, "cow.bin")
	cow := make([]byte, 4*testBlock)
	copy(cow[testBlock:2*testBlock], bytes.Repeat([]byte{9}, int(testBlock)))
	if err := os.WriteFile(cowPath, cow, 0o644); err != nil {
		t.Fatal(err)
	}

	dirty := roaring.New()
	dirty.Add(1)

	withBitmap := filepath.Join(dir, "with")
	if _, _, err := Chunkify(ctx, ChunkifyOpts{
		In: cowPath, OutDir: withBitmap, BuildID: uuid.New(), BlockSize: testBlock,
		ParentDir: templateDir, Dirty: dirty,
	}); err != nil {
		t.Fatalf("Chunkify(cow): %v", err)
	}

	want := []byte{1, 9, 3, 4}
	got := readMerged(t, withBitmap, templateDir)
	for i, fill := range want {
		block := got[int64(i)*testBlock : int64(i+1)*testBlock]
		if !bytes.Equal(block, bytes.Repeat([]byte{fill}, int(testBlock))) {
			t.Errorf("block %d = %v..., want all %d", i, block[:4], fill)
		}
	}

	// And the failure mode it prevents: the same file, no bitmap.
	withoutBitmap := filepath.Join(dir, "without")
	if _, _, err := Chunkify(ctx, ChunkifyOpts{
		In: cowPath, OutDir: withoutBitmap, BuildID: uuid.New(), BlockSize: testBlock,
		ParentDir: templateDir,
	}); err != nil {
		t.Fatalf("Chunkify(cow, no bitmap): %v", err)
	}
	naive := readMerged(t, withoutBitmap, templateDir)
	if bytes.Equal(naive, got) {
		t.Fatal("the bitmap made no difference; this test can no longer observe the bug")
	}
	if !bytes.Equal(naive[:testBlock], make([]byte, testBlock)) {
		t.Error("expected the bitmap-less diff to zero block 0")
	}
}

// Clean blocks must cost nothing in the data file. A checkpoint of a machine
// that wrote a megabyte must not upload the whole disk.
func TestCowDiffStoresOnlyDirtyBlocks(t *testing.T) {
	dir := t.TempDir()

	templateIn := writeBlocks(t, dir, 1, 2, 3, 4, 5, 6, 7, 8)
	templateDir := buildTemplate(t, dir, templateIn)

	cowPath := filepath.Join(dir, "cow.bin")
	cow := make([]byte, 8*testBlock)
	copy(cow[3*testBlock:4*testBlock], bytes.Repeat([]byte{99}, int(testBlock)))
	if err := os.WriteFile(cowPath, cow, 0o644); err != nil {
		t.Fatal(err)
	}

	dirty := roaring.New()
	dirty.Add(3)

	out := filepath.Join(dir, "diff")
	_, stats, err := Chunkify(context.Background(), ChunkifyOpts{
		In: cowPath, OutDir: out, BuildID: uuid.New(), BlockSize: testBlock,
		ParentDir: templateDir, Dirty: dirty,
	})
	if err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	if stats.PackedBytes != testBlock {
		t.Errorf("packed %d bytes for one dirty block, want %d", stats.PackedBytes, testBlock)
	}

	info, err := os.Stat(filepath.Join(out, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != testBlock {
		t.Errorf("data file is %d bytes, want %d", info.Size(), testBlock)
	}
}

// A block the guest deliberately zeroed must read back as zeros, not fall
// through to the template. It is dirty, so it is read; being all-zero it
// becomes a gap, and a gap means zeros regardless of what the parent holds.
func TestCowDiffKeepsADeliberatelyZeroedBlockZero(t *testing.T) {
	dir := t.TempDir()

	templateIn := writeBlocks(t, dir, 1, 2, 3)
	templateDir := buildTemplate(t, dir, templateIn)

	cowPath := filepath.Join(dir, "cow.bin")
	if err := os.WriteFile(cowPath, make([]byte, 3*testBlock), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty := roaring.New()
	dirty.Add(1) // the guest zeroed block 1

	out := filepath.Join(dir, "diff")
	if _, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: cowPath, OutDir: out, BuildID: uuid.New(), BlockSize: testBlock,
		ParentDir: templateDir, Dirty: dirty,
	}); err != nil {
		t.Fatalf("Chunkify: %v", err)
	}

	got := readMerged(t, out, templateDir)
	if !bytes.Equal(got[testBlock:2*testBlock], make([]byte, testBlock)) {
		t.Error("a zeroed block fell through to the template")
	}
	if !bytes.Equal(got[:testBlock], bytes.Repeat([]byte{1}, int(testBlock))) {
		t.Error("an untouched block did not come from the template")
	}
}

// Runs of clean blocks must coalesce, or a lightly-written multi-gigabyte disk
// produces a mapping per block and a header larger than the data.
func TestCowDiffCoalescesCleanRuns(t *testing.T) {
	dir := t.TempDir()

	fills := make([]byte, 64)
	for i := range fills {
		fills[i] = byte(i + 1)
	}
	templateIn := writeBlocks(t, dir, fills...)
	templateDir := buildTemplate(t, dir, templateIn)

	cowPath := filepath.Join(dir, "cow.bin")
	cow := make([]byte, int64(len(fills))*testBlock)
	copy(cow[32*testBlock:33*testBlock], bytes.Repeat([]byte{200}, int(testBlock)))
	if err := os.WriteFile(cowPath, cow, 0o644); err != nil {
		t.Fatal(err)
	}

	dirty := roaring.New()
	dirty.Add(32)

	out := filepath.Join(dir, "diff")
	_, stats, err := Chunkify(context.Background(), ChunkifyOpts{
		In: cowPath, OutDir: out, BuildID: uuid.New(), BlockSize: testBlock,
		ParentDir: templateDir, Dirty: dirty,
	})
	if err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	// Two clean runs around one dirty block.
	if stats.Mappings > 3 {
		t.Errorf("%d mappings for 64 blocks with one dirty; clean runs did not coalesce",
			stats.Mappings)
	}
}

// Dirty without a parent has nothing for its clean blocks to point at, so it
// must be rejected rather than quietly producing a template full of holes.
func TestChunkifyRejectsDirtyWithoutAParent(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1, 2)

	_, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: filepath.Join(dir, "out"), BuildID: uuid.New(),
		BlockSize: testBlock, Dirty: roaring.New(),
	})
	if err == nil {
		t.Error("Dirty was accepted without ParentDir")
	}
}

// readMerged reads a diff through its parent.
//
// A LocalBuild deliberately refuses to chase a foreign build id, so the merged
// view is assembled the way a wake assembles it: both builds served from
// storage, with the parent attached.
func readMerged(t *testing.T, diffDir, parentDir string) []byte {
	t.Helper()
	ctx := context.Background()

	store := newFakeStore()
	parentID := upload(t, store, parentDir)
	diffID := upload(t, store, diffDir)

	cacheRoot := filepath.Join(t.TempDir(), "cache")
	parent, err := OpenRemoteBuild(ctx, store, parentID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(parent): %v", err)
	}
	defer parent.Close()

	diff, err := OpenRemoteBuild(ctx, store, diffID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(diff): %v", err)
	}
	defer diff.Close()
	diff.SetParent(parent)

	return readWhole(t, diff)
}

// upload copies a build directory into a fake store and returns its id.
func upload(t *testing.T, s *fakeStore, dir string) uuid.UUID {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "header"))
	if err != nil {
		t.Fatal(err)
	}
	header, err := Deserialize(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	id := header.Metadata.BuildId

	data, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	s.objects[id.String()+"/header"] = raw
	s.objects[id.String()+"/data"] = data
	return id
}
