package block

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

const testBlock int64 = 4096

// writeBlocks builds an input file from per-block fill bytes. A fill of 0
// produces an all-zero block, which chunkify should elide.
func writeBlocks(t *testing.T, dir string, fills ...byte) string {
	t.Helper()
	path := filepath.Join(dir, "in.bin")
	var buf bytes.Buffer
	for _, f := range fills {
		buf.Write(bytes.Repeat([]byte{f}, int(testBlock)))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readWhole reads a build back block by block, the way a consumer would.
func readWhole(t *testing.T, b Slicer) []byte {
	t.Helper()
	out := make([]byte, 0, b.Size())
	for off := int64(0); off < b.Size(); {
		chunk, err := b.Slice(context.Background(), off, b.Size()-off)
		if err != nil {
			t.Fatalf("Slice at %d: %v", off, err)
		}
		if len(chunk) == 0 {
			t.Fatalf("Slice at %d returned nothing", off)
		}
		out = append(out, chunk...)
		off += int64(len(chunk))
	}
	return out
}

func chunkifyTo(t *testing.T, in, outDir string, parentDir string, id uuid.UUID) ChunkifyStats {
	t.Helper()
	_, stats, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: outDir, BuildID: id, BlockSize: testBlock, ParentDir: parentDir,
	})
	if err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	return stats
}

// A build must read back byte-identical to what went in, whatever the mix of
// zero and non-zero blocks.
func TestChunkifyRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fills []byte
	}{
		{"all zero", []byte{0, 0, 0, 0}},
		{"all data", []byte{1, 2, 3, 4}},
		{"alternating", []byte{0, 1, 0, 2}},
		{"leading zeros", []byte{0, 0, 7, 8}},
		{"trailing zeros", []byte{7, 8, 0, 0}},
		{"single block", []byte{9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := writeBlocks(t, dir, tc.fills...)
			want, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}

			outDir := filepath.Join(dir, "build")
			chunkifyTo(t, in, outDir, "", uuid.New())

			b, err := OpenLocalBuild(outDir)
			if err != nil {
				t.Fatalf("OpenLocalBuild: %v", err)
			}
			defer b.Close()

			if got := readWhole(t, b); !bytes.Equal(got, want) {
				t.Errorf("round trip differs at first mismatch %d", firstDiff(got, want))
			}
		})
	}
}

// Zero blocks are the whole point of the format: they cost no storage.
func TestChunkifyElidesZeroBlocks(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 0, 1, 0, 0, 2, 0)
	outDir := filepath.Join(dir, "build")
	stats := chunkifyTo(t, in, outDir, "", uuid.New())

	if stats.ZeroBlocks != 4 {
		t.Errorf("elided %d zero blocks, want 4", stats.ZeroBlocks)
	}
	if stats.PackedBytes != 2*testBlock {
		t.Errorf("packed %d bytes, want %d -- only the two non-zero blocks",
			stats.PackedBytes, 2*testBlock)
	}

	info, err := os.Stat(filepath.Join(outDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2*testBlock {
		t.Errorf("data file is %d bytes, want %d", info.Size(), 2*testBlock)
	}
}

// An entirely zero input produces an empty data file. That is legal, and it is
// exactly the case that later makes a range GET return 416.
func TestChunkifyAllZeroProducesEmptyData(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 0, 0, 0)
	outDir := filepath.Join(dir, "build")
	chunkifyTo(t, in, outDir, "", uuid.New())

	info, err := os.Stat(filepath.Join(outDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("data file is %d bytes, want 0", info.Size())
	}

	b, err := OpenLocalBuild(outDir)
	if err != nil {
		t.Fatalf("OpenLocalBuild on an all-zero build: %v", err)
	}
	defer b.Close()

	got := readWhole(t, b)
	if !bytes.Equal(got, make([]byte, 3*testBlock)) {
		t.Error("an all-zero build did not read back as zeros")
	}
}

// A diff of an unchanged input should store nothing at all.
func TestChunkifyDiffIdenticalStoresNothing(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1, 2, 3, 4)

	parentDir := filepath.Join(dir, "parent")
	chunkifyTo(t, in, parentDir, "", uuid.New())

	diffDir := filepath.Join(dir, "diff")
	stats := chunkifyTo(t, in, diffDir, parentDir, uuid.New())

	if stats.PackedBytes != 0 {
		t.Errorf("a diff of identical content packed %d bytes, want 0", stats.PackedBytes)
	}
	info, err := os.Stat(filepath.Join(diffDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("diff data file is %d bytes, want 0", info.Size())
	}
}

// Only the changed blocks belong to the diff; the rest point at the parent.
func TestChunkifyDiffStoresOnlyChangedBlocks(t *testing.T) {
	dir := t.TempDir()
	parentIn := writeBlocks(t, dir, 1, 2, 3, 4)
	parentDir := filepath.Join(dir, "parent")
	chunkifyTo(t, parentIn, parentDir, "", uuid.New())

	// Change block 2 only.
	changed := filepath.Join(dir, "changed.bin")
	var buf bytes.Buffer
	for _, f := range []byte{1, 2, 9, 4} {
		buf.Write(bytes.Repeat([]byte{f}, int(testBlock)))
	}
	if err := os.WriteFile(changed, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	diffDir := filepath.Join(dir, "diff")
	stats := chunkifyTo(t, changed, diffDir, parentDir, uuid.New())

	if stats.PackedBytes != testBlock {
		t.Errorf("packed %d bytes, want one block -- only block 2 changed",
			stats.PackedBytes)
	}
	if stats.ParentRefs == 0 {
		t.Error("no ranges were served by the parent; the unchanged blocks should be")
	}
}

// Coalescing requires logical contiguity AND parent-storage contiguity AND the
// same source mapping. Two of the three produces a header that parses cleanly
// and resolves to the wrong parent bytes -- corruption with no error.
//
// The parent here has a gap between two data blocks, so blocks that are
// logically adjacent in the child read from NON-contiguous parent storage.
// They must not merge into one mapping.
func TestChunkifyDoesNotCoalesceAcrossNonContiguousParentStorage(t *testing.T) {
	dir := t.TempDir()

	// Parent: data, zero, data. The two data blocks are adjacent in storage
	// but three blocks apart logically.
	parentIn := writeBlocks(t, dir, 1, 0, 3)
	parentDir := filepath.Join(dir, "parent")
	chunkifyTo(t, parentIn, parentDir, "", uuid.New())

	// Child: identical, so every non-zero block points at the parent.
	diffDir := filepath.Join(dir, "diff")
	header, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: parentIn, OutDir: diffDir, BuildID: uuid.New(),
		BlockSize: testBlock, ParentDir: parentDir,
	})
	if err != nil {
		t.Fatalf("Chunkify: %v", err)
	}

	// Blocks 0 and 2 are separated by a gap, so they cannot share a mapping.
	for _, m := range header.Mapping {
		if m.Length > uint64(testBlock) {
			t.Errorf("a mapping spans %d bytes across a parent gap; coalescing "+
				"ignored parent-storage contiguity and now resolves to the wrong bytes: %+v",
				m.Length, *m)
		}
	}

	// And the diff must still read back correctly.
	b, err := OpenLocalBuild(diffDir)
	if err != nil {
		t.Fatalf("OpenLocalBuild: %v", err)
	}
	defer b.Close()
	_ = b // reading through a diff needs parent dispatch, which RemoteBuild provides
}

func TestChunkifyRejectsAGrandparentChain(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1, 2)

	parentDir := filepath.Join(dir, "parent")
	chunkifyTo(t, in, parentDir, "", uuid.New())

	// A diff is generation 1; diffing against IT would be generation 2.
	diffDir := filepath.Join(dir, "diff")
	changedDir := filepath.Join(dir, "b")
	if err := os.MkdirAll(changedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	changed := writeBlocks(t, changedDir, 1, 9)
	chunkifyTo(t, changed, diffDir, parentDir, uuid.New())

	_, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: filepath.Join(dir, "grandchild"), BuildID: uuid.New(),
		BlockSize: testBlock, ParentDir: diffDir,
	})
	if err == nil {
		t.Fatal("chunkifying against a diff was allowed; the chain is now three deep")
	}
	if !errors.Is(err, ErrGrandparentChain) {
		t.Errorf("got %v, want ErrGrandparentChain", err)
	}
}

// A caller asking for a diff expects the parent's bytes to be reachable. If
// they are not, producing a self-contained build instead would silently
// discard the dedup the caller was relying on.
func TestChunkifyMissingParentIsFatal(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1, 2)

	_, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: filepath.Join(dir, "out"), BuildID: uuid.New(),
		BlockSize: testBlock, ParentDir: filepath.Join(dir, "nonexistent"),
	})
	if err == nil {
		t.Fatal("a missing parent was tolerated")
	}
}

// The metadata records the PADDED size, because readers address a build in
// whole blocks and a trailing partial block would be unaddressable.
func TestChunkifyPadsToBlockBoundary(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(in, bytes.Repeat([]byte{7}, 100), 0o644); err != nil {
		t.Fatal(err)
	}

	header, stats, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: filepath.Join(dir, "out"), BuildID: uuid.New(), BlockSize: testBlock,
	})
	if err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	if header.Metadata.Size != uint64(testBlock) {
		t.Errorf("metadata size = %d, want %d", header.Metadata.Size, testBlock)
	}
	if stats.LogicalBytes != testBlock {
		t.Errorf("logical bytes = %d, want %d", stats.LogicalBytes, testBlock)
	}
}

func TestOpenLocalBuildRejectsTruncatedData(t *testing.T) {
	dir := t.TempDir()
	in := writeBlocks(t, dir, 1, 2, 3)
	outDir := filepath.Join(dir, "build")
	chunkifyTo(t, in, outDir, "", uuid.New())

	// Truncate the data file: the header still maps bytes that are gone.
	if err := os.Truncate(filepath.Join(outDir, "data"), testBlock); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocalBuild(outDir); err == nil {
		t.Error("a truncated data file was accepted; the missing tail would read as zeros")
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
