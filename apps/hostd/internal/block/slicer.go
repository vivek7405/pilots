package block

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrBytesNotAvailable reports that a source cannot serve a range.
//
// This is control flow, not a failure: an Overlay reads it as "not written
// here yet, fall through to the template underneath".
var ErrBytesNotAvailable = errors.New("block: bytes not available")

// Slicer serves bytes for a logical range of a build.
//
// Prefault is part of the interface rather than a detail of whoever consumes
// it: only the source knows how its bytes are stored, and therefore how to
// pull them in one coalesced operation instead of thousands of small ones.
type Slicer interface {
	// Slice returns up to length bytes at the logical offset. A short return
	// is legal; callers loop.
	Slice(ctx context.Context, off, length int64) ([]byte, error)
	BlockSize() int64
	Size() int64
	// Prefault warms the source. Without it, a lazily-faulted guest issues one
	// round trip per page.
	Prefault(ctx context.Context) error
}

// HeaderedSlicer is a Slicer that can describe its own layout, which the CoW
// cache needs in order to replay a build into itself.
type HeaderedSlicer interface {
	Slicer
	Header() *Header
}

// LocalBuild reads a build from a directory holding "header" and "data".
//
// Used for the golden template on disk and as chunkify's parent. Unlike the
// remote build it has no parent dispatch: a local build must be
// self-contained, so a mapping naming another build is an error rather than
// something to chase.
type LocalBuild struct {
	header *Header
	data   *os.File
	dir    string
}

// OpenLocalBuild reads a build directory.
func OpenLocalBuild(dir string) (*LocalBuild, error) {
	headerFile, err := os.Open(filepath.Join(dir, "header"))
	if err != nil {
		return nil, fmt.Errorf("block: open header in %s: %w", dir, err)
	}
	defer headerFile.Close()

	header, err := Deserialize(headerFile)
	if err != nil {
		return nil, err
	}

	data, err := os.Open(filepath.Join(dir, "data"))
	if err != nil {
		return nil, fmt.Errorf("block: open data in %s: %w", dir, err)
	}

	b := &LocalBuild{header: header, data: data, dir: dir}
	if err := b.validateDataSize(); err != nil {
		data.Close()
		return nil, err
	}
	return b, nil
}

// validateDataSize checks the data file can satisfy every self-mapping.
//
// A truncated data file otherwise reads back as zeros for the missing tail,
// which is indistinguishable from legitimately elided blocks -- silent
// corruption rather than an error.
func (b *LocalBuild) validateDataSize() error {
	info, err := b.data.Stat()
	if err != nil {
		return fmt.Errorf("block: stat data in %s: %w", b.dir, err)
	}

	var needed int64
	for _, m := range b.header.Mapping {
		if m.BuildId != b.header.Metadata.BuildId {
			continue
		}
		if end := int64(m.BuildStorageOffset + m.Length); end > needed {
			needed = end
		}
	}
	if info.Size() < needed {
		return fmt.Errorf("block: data in %s is %d bytes but the header maps %d",
			b.dir, info.Size(), needed)
	}
	return nil
}

func (b *LocalBuild) Header() *Header  { return b.header }
func (b *LocalBuild) BlockSize() int64 { return int64(b.header.Metadata.BlockSize) }
func (b *LocalBuild) Size() int64      { return int64(b.header.Metadata.Size) }

// Prefault is a no-op: the bytes are already local.
func (b *LocalBuild) Prefault(context.Context) error { return nil }

func (b *LocalBuild) Close() error { return b.data.Close() }

// Slice serves a logical range.
func (b *LocalBuild) Slice(_ context.Context, off, length int64) ([]byte, error) {
	storageOff, mappedLen, buildID, err := b.header.GetShiftedMapping(uint64(off))
	if err != nil {
		return nil, err
	}

	if length > int64(mappedLen) {
		length = int64(mappedLen)
	}

	if buildID == nil {
		// A gap: never stored because it was all zeros.
		return make([]byte, length), nil
	}
	if *buildID != b.header.Metadata.BuildId {
		return nil, fmt.Errorf("block: local build %s has a mapping at %d naming "+
			"build %s; a local build must be self-contained",
			b.header.Metadata.BuildId, off, *buildID)
	}

	out := make([]byte, length)
	n, err := b.data.ReadAt(out, int64(storageOff))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("block: read data at %d: %w", storageOff, err)
	}
	// Short read past the end of the data file: the remainder is zeros, which
	// is what an elided tail means.
	for i := n; i < len(out); i++ {
		out[i] = 0
	}
	return out, nil
}

// blockAt reads the block backing a logical offset, for chunkify's comparison.
//
// Returns the mapping that covers the offset (nil for a gap) and the storage
// offset the block was read from, which the caller needs to decide whether a
// run can be coalesced.
func (b *LocalBuild) blockAt(off int64, buf []byte) (*BuildMap, int64, error) {
	m, storageOff, err := b.mappingAt(off)
	if err != nil {
		return nil, 0, err
	}
	if m == nil {
		// A parent gap is all zeros.
		for i := range buf {
			buf[i] = 0
		}
		return nil, 0, nil
	}

	n, err := b.data.ReadAt(buf, storageOff)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("block: read parent at %d: %w", storageOff, err)
	}
	for i := n; i < len(buf); i++ {
		buf[i] = 0
	}

	return m, storageOff, nil
}

// mappingAt resolves a logical offset to its mapping and storage offset
// WITHOUT reading the block.
//
// The copy-on-write path needs the pointer for every clean block and the bytes
// for none of them. Reading anyway would mean streaming the whole template on
// every checkpoint -- exactly the cost the dirty bitmap exists to avoid.
func (b *LocalBuild) mappingAt(off int64) (*BuildMap, int64, error) {
	storageOff, _, buildID, err := b.header.GetShiftedMapping(uint64(off))
	if err != nil {
		return nil, 0, err
	}
	if buildID == nil {
		return nil, 0, nil
	}
	if *buildID != b.header.Metadata.BuildId {
		// The parent itself points at a third build, so the chain is deeper
		// than template -> diff. Comparing against it would encode our diff
		// against bytes that cannot be reproduced from the parent alone.
		return nil, 0, fmt.Errorf("%w: parent build %s maps offset %d to build %s",
			ErrGrandparentChain, b.header.Metadata.BuildId, off, *buildID)
	}
	return b.mappingCovering(off), int64(storageOff), nil
}

// mappingCovering returns the mapping containing a logical offset.
func (b *LocalBuild) mappingCovering(off int64) *BuildMap {
	for _, m := range b.header.Mapping {
		if uint64(off) >= m.Offset && uint64(off) < m.Offset+m.Length {
			return m
		}
	}
	return nil
}
