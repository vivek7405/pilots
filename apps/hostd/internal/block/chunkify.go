package block

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
)

// DefaultBlockSize matches the guest page size, which is what makes a memory
// image chunk cleanly.
const DefaultBlockSize int64 = 4096

// ChunkifyOpts describes one build to produce.
type ChunkifyOpts struct {
	// In is the file to chunk: a memory image or a disk.
	In string
	// OutDir receives "header" and "data".
	OutDir string
	// BuildID names the build. Generated when empty.
	BuildID uuid.UUID
	// BlockSize defaults to DefaultBlockSize.
	BlockSize int64
	// ParentDir, when set, produces a DIFF against the build in that
	// directory: blocks identical to the parent are recorded as pointers and
	// cost no storage. Absent, the result is a self-contained template.
	ParentDir string
	// Dirty, when set, names the only blocks In is allowed to speak for.
	// Requires ParentDir.
	//
	// This is what makes a copy-on-write file chunkifiable at all. A CoW
	// cache is SPARSE: a block the guest never wrote reads back as zeros,
	// which is indistinguishable from a block it deliberately zeroed. Diffing
	// it byte-for-byte would therefore record every untouched block as "zeros,
	// mine" and shadow the template's real content with holes -- a rootfs that
	// parses, mounts, and is empty. With the bitmap, clean blocks become
	// parent pointers without being read, which is also why a checkpoint costs
	// the size of the writes rather than the size of the disk.
	Dirty *roaring.Bitmap
}

// ChunkifyStats reports what a build cost.
type ChunkifyStats struct {
	LogicalBytes int64 // the padded size of the input
	PackedBytes  int64 // what actually landed in the data file
	Mappings     int
	ParentRefs   int // ranges served by the parent, stored nowhere
	ZeroBlocks   int
}

// Chunkify writes a content-addressed build.
//
// This is a library function, not only a CLI. The predecessor kept it inside
// main and shelled out to a binary, which meant its own block tests had to
// hand-roll headers -- so the format was never exercised by the code that
// produces it -- and the concurrency limit bounded processes rather than the
// memory each one maps.
func Chunkify(ctx context.Context, opts ChunkifyOpts) (*Header, ChunkifyStats, error) {
	var stats ChunkifyStats

	blockSize := opts.BlockSize
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	buildID := opts.BuildID
	if buildID == uuid.Nil {
		buildID = uuid.New()
	}

	src, err := os.Open(opts.In)
	if err != nil {
		return nil, stats, fmt.Errorf("block: open %s: %w", opts.In, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return nil, stats, fmt.Errorf("block: stat %s: %w", opts.In, err)
	}

	// The logical size is padded up to a whole number of blocks, and that
	// padded size is what the metadata records -- readers address the build in
	// blocks, so a trailing partial block would be unaddressable.
	size := info.Size()
	if rem := size % blockSize; rem != 0 {
		size += blockSize - rem
	}
	stats.LogicalBytes = size

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, stats, fmt.Errorf("block: mkdir %s: %w", opts.OutDir, err)
	}
	dataPath := filepath.Join(opts.OutDir, "data")
	dst, err := os.Create(dataPath)
	if err != nil {
		return nil, stats, fmt.Errorf("block: create %s: %w", dataPath, err)
	}
	defer dst.Close()

	var (
		metadata *Metadata
		mapping  []*BuildMap
	)

	if opts.ParentDir == "" {
		if opts.Dirty != nil {
			return nil, stats, errors.New(
				"block: Dirty requires ParentDir; clean blocks have nowhere to point")
		}
		metadata = NewTemplateMetadata(buildID, uint64(blockSize), uint64(size))
		mapping, err = writeSparse(ctx, src, dst, size, blockSize, buildID, &stats)
	} else {
		var parent *LocalBuild
		// A missing parent is fatal, never a silent fall back to a
		// self-contained build: the caller asked for a diff because it
		// expects the parent's bytes to still be reachable.
		parent, err = OpenLocalBuild(opts.ParentDir)
		if err != nil {
			return nil, stats, fmt.Errorf("block: open parent build: %w", err)
		}
		defer parent.Close()

		metadata = parent.Header().Metadata.NextGeneration(buildID)
		metadata.Size = uint64(size)
		mapping, err = writeDiff(ctx, src, dst, parent, opts.Dirty, size, blockSize, buildID, &stats)
	}
	if err != nil {
		return nil, stats, err
	}

	header, err := NewHeader(metadata, mapping)
	if err != nil {
		return nil, stats, err
	}
	stats.Mappings = len(header.Mapping)

	raw, err := Serialize(header.Metadata, header.Mapping)
	if err != nil {
		return nil, stats, err
	}
	if err := os.WriteFile(filepath.Join(opts.OutDir, "header"), raw, 0o644); err != nil {
		return nil, stats, fmt.Errorf("block: write header: %w", err)
	}
	return header, stats, nil
}

// writeSparse produces a self-contained build, eliding all-zero blocks.
//
// A zero block is simply not stored: it becomes a gap in the mapping, which a
// reader fills with zeros. Runs of consecutive non-zero blocks coalesce into
// one mapping so a mostly-contiguous image does not produce a mapping per
// block.
func writeSparse(ctx context.Context, src io.Reader, dst io.Writer, size, blockSize int64,
	buildID uuid.UUID, stats *ChunkifyStats) ([]*BuildMap, error) {

	var (
		mapping  []*BuildMap
		packed   int64
		runStart int64 = -1
		runBytes int64
	)
	buf := make([]byte, blockSize)
	zero := make([]byte, blockSize)

	closeRun := func() {
		if runStart < 0 {
			return
		}
		mapping = append(mapping, &BuildMap{
			Offset:             uint64(runStart),
			Length:             uint64(runBytes),
			BuildId:            buildID,
			BuildStorageOffset: uint64(packed - runBytes),
		})
		runStart, runBytes = -1, 0
	}

	for off := int64(0); off < size; off += blockSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := readBlock(src, buf); err != nil {
			return nil, err
		}

		if bytes.Equal(buf, zero) {
			stats.ZeroBlocks++
			closeRun()
			continue
		}

		if _, err := dst.Write(buf); err != nil {
			return nil, fmt.Errorf("block: write data at %d: %w", off, err)
		}
		packed += blockSize
		if runStart < 0 {
			runStart = off
		}
		runBytes += blockSize
	}
	closeRun()

	stats.PackedBytes = packed
	return mapping, nil
}

// runKind distinguishes the two kinds of coalescable run in a diff.
type runKind int

const (
	runNone runKind = iota
	runOwn
	runParent
)

// writeDiff produces a build relative to a parent.
//
// Three outcomes per block: all-zero becomes a gap; identical to the parent
// becomes a pointer at the parent's bytes and costs no storage; anything else
// is appended to our own data file.
//
// dirty, when non-nil, restricts which blocks are read at all: everything
// outside it is a parent pointer by definition. See ChunkifyOpts.Dirty for why
// a copy-on-write file cannot be diffed without it.
func writeDiff(ctx context.Context, src io.ReaderAt, dst io.Writer, parent *LocalBuild,
	dirty *roaring.Bitmap, size, blockSize int64, buildID uuid.UUID,
	stats *ChunkifyStats) ([]*BuildMap, error) {

	var (
		mapping []*BuildMap
		packed  int64

		kind        = runNone
		runStart    int64
		runBytes    int64
		runStorage  int64
		runParentAt int64 // the parent mapping this run is reading through
		runBuild    uuid.UUID
	)
	buf := make([]byte, blockSize)
	zero := make([]byte, blockSize)
	parentBuf := make([]byte, blockSize)

	closeRun := func() {
		if kind == runNone {
			return
		}
		mapping = append(mapping, &BuildMap{
			Offset:             uint64(runStart),
			Length:             uint64(runBytes),
			BuildId:            runBuild,
			BuildStorageOffset: uint64(runStorage),
		})
		kind = runNone
	}

	// extendParent coalesces a parent-served block onto the open run.
	//
	// All THREE conditions are required. Logical contiguity alone is not
	// enough: extending Length only stays correct while the parent's storage
	// is contiguous too AND both blocks read through the same parent mapping.
	// Dropping either of the latter produces a header that parses cleanly and
	// resolves to the WRONG parent bytes -- corruption with no error anywhere.
	extendParent := func(off int64, pm *BuildMap, parentStorageOff int64) bool {
		return kind == runParent &&
			runStart+runBytes == off &&
			runStorage+runBytes == parentStorageOff &&
			runParentAt == int64(pm.Offset)
	}
	openParent := func(off int64, pm *BuildMap, parentStorageOff int64) {
		closeRun()
		kind = runParent
		runStart = off
		runBytes = blockSize
		runStorage = parentStorageOff
		runParentAt = int64(pm.Offset)
		runBuild = pm.BuildId
		stats.ParentRefs++
	}

	for off := int64(0); off < size; off += blockSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if dirty != nil && !dirty.Contains(uint32(off/blockSize)) {
			// Never written: the parent still speaks for this block. Read
			// nothing -- neither ours (it would be a hole) nor the parent's.
			pm, parentStorageOff, err := parent.mappingAt(off)
			if err != nil {
				return nil, err
			}
			if pm == nil {
				// The parent has a gap here, so zeros are already correct.
				stats.ZeroBlocks++
				closeRun()
				continue
			}
			if extendParent(off, pm, parentStorageOff) {
				runBytes += blockSize
				continue
			}
			openParent(off, pm, parentStorageOff)
			continue
		}

		if err := readBlockAt(src, buf, off); err != nil {
			return nil, err
		}

		if bytes.Equal(buf, zero) {
			// A gap reads back as zeros whatever the parent holds, so this is
			// correct for a deliberately zeroed block too.
			stats.ZeroBlocks++
			closeRun()
			continue
		}

		pm, parentStorageOff, err := parent.blockAt(off, parentBuf)
		if err != nil {
			return nil, err
		}

		if pm != nil && bytes.Equal(buf, parentBuf) {
			// Identical to the parent: record a pointer, store nothing.
			if extendParent(off, pm, parentStorageOff) {
				runBytes += blockSize
				continue
			}
			openParent(off, pm, parentStorageOff)
			continue
		}

		// Differs from the parent: our own bytes.
		if _, err := dst.Write(buf); err != nil {
			return nil, fmt.Errorf("block: write data at %d: %w", off, err)
		}
		if kind == runOwn && runStart+runBytes == off {
			runBytes += blockSize
			packed += blockSize
			continue
		}
		closeRun()
		kind = runOwn
		runStart = off
		runBytes = blockSize
		runStorage = packed
		runBuild = buildID
		packed += blockSize
	}
	closeRun()

	stats.PackedBytes = packed
	return mapping, nil
}

// readBlockAt fills buf from an absolute offset, zero-padding past end of file.
func readBlockAt(src io.ReaderAt, buf []byte, off int64) error {
	n, err := src.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("block: read source at %d: %w", off, err)
	}
	// Past the end of the source -- or inside a hole the filesystem never
	// allocated. Either way the padding is zeros.
	for i := n; i < len(buf); i++ {
		buf[i] = 0
	}
	return nil
}

// readBlock fills buf, zero-padding a short read at end of file.
func readBlock(src io.Reader, buf []byte) error {
	n, err := io.ReadFull(src, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		// Past the end of the source: the padding is zeros, which is exactly
		// what the all-zero test below should see.
		for i := n; i < len(buf); i++ {
			buf[i] = 0
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("block: read: %w", err)
	}
	return nil
}
