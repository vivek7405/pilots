package block

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
)

// ObjectStore is the storage surface a remote build needs.
//
// An interface rather than the concrete client so the block layer can be
// tested against a fake -- the predecessor could not, and its diff-chain test
// hand-stitched the merged view instead of exercising the parent dispatch that
// actually breaks a wake.
type ObjectStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	// GetRange must report an HTTP 416 as an error satisfying
	// errors.Is(err, ErrRangeNotSatisfiable).
	GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error)
}

// ErrRangeNotSatisfiable reports a range request past the end of an object.
//
// Not an edge case: a build whose every block was elided has a ZERO-LENGTH
// data object, so any range against it is unsatisfiable. That means "these
// blocks are zeros", and treating it as a failure kills the wake.
var ErrRangeNotSatisfiable = errors.New("block: range not satisfiable")

// RemoteBuild serves a build out of object storage, caching what it fetches.
type RemoteBuild struct {
	store   ObjectStore
	header  *Header
	buildID uuid.UUID

	// parent serves ranges this build did not store because they were
	// identical to it.
	parent Slicer

	dir        string
	data       *os.File
	packedSize int64

	mu       sync.Mutex
	cached   *roaring.Bitmap // storage blocks already on local disk
	complete bool            // every storage block is on disk and the marker is written
}

// completeMarker is written next to "data" once every storage block has landed.
//
// The data file is TRUNCATED to its full packed size when the cache is created,
// so its size says nothing about how much of it was actually downloaded. A
// handler that exits partway -- a failed bulk fetch, a SIGKILL, a host reboot --
// leaves a full-length file holding holes, and trusting the size alone makes
// the next open serve those holes as zeros with no error anywhere. The marker
// is the only thing that distinguishes "complete" from "the right length".
const completeMarker = "data.complete"

// OpenRemoteBuild fetches a build's header and prepares its local cache.
func OpenRemoteBuild(ctx context.Context, store ObjectStore, buildID uuid.UUID, cacheRoot string) (*RemoteBuild, error) {
	raw, err := store.Get(ctx, buildID.String()+"/header")
	if err != nil {
		return nil, fmt.Errorf("block: fetch header for %s: %w", buildID, err)
	}
	header, err := Deserialize(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(cacheRoot, buildID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("block: mkdir %s: %w", dir, err)
	}
	// Write the header alongside the data. A later chunkify needs to open this
	// build as a parent, and it must be able to do that from disk alone,
	// without object-storage credentials.
	if err := os.WriteFile(filepath.Join(dir, "header"), raw, 0o644); err != nil {
		return nil, fmt.Errorf("block: cache header: %w", err)
	}

	b := &RemoteBuild{
		store: store, header: header, buildID: buildID,
		dir: dir, cached: roaring.New(),
	}

	// The packed size is the extent of what this build actually stores, which
	// is the end of its furthest SELF-pointing mapping. Parent-pointing ranges
	// live in the parent's data file and contribute nothing here.
	for _, m := range header.Mapping {
		if m.BuildId != header.Metadata.BuildId {
			continue
		}
		if end := int64(m.BuildStorageOffset + m.Length); end > b.packedSize {
			b.packedSize = end
		}
	}

	dataPath := filepath.Join(dir, "data")
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("block: open data cache: %w", err)
	}
	b.data = f

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("block: stat data cache: %w", err)
	}

	_, markerErr := os.Stat(filepath.Join(dir, completeMarker))
	if b.packedSize > 0 && markerErr == nil && info.Size() >= b.packedSize {
		// Already complete on disk -- a same-host wake, or a second restore.
		// Marking every block cached up front matters more than it looks:
		// without it a wake re-downloads a file it already has, and faults
		// land at a rate that leaves the guest deadlocked waiting for pages.
		//
		// The marker, not the size, is what licenses this: see completeMarker.
		b.cached.AddRange(0, uint64(TotalBlocks(b.packedSize, b.BlockSize())))
		b.complete = true
	} else if err := f.Truncate(b.packedSize); err != nil {
		f.Close()
		return nil, fmt.Errorf("block: truncate data cache: %w", err)
	}

	return b, nil
}

// SetParent attaches the build that serves this one's unchanged ranges.
//
// It reports a parent that is not the one this diff was encoded against. The
// caller decides what to do about it, but it must not be ignored: a diff's
// parent-pointing ranges name a logical offset, not bytes, so attaching a
// DIFFERENT build resolves them to whatever that build happens to hold at the
// same offset. Nothing downstream can notice -- the header parses, every range
// resolves, and the guest simply comes back with other memory or another
// machine's disk. Which template a host has is not fleet-wide state: a host
// that has never built one, or whose cache was cleared, mints a fresh template
// with fresh ids, so this is the check that stands between a wake on such a
// host and silent corruption.
//
// The parent is attached either way, so a caller that genuinely wants an
// unchecked parent -- a test stitching builds by hand -- can discard the error.
func (b *RemoteBuild) SetParent(parent Slicer) error {
	b.parent = parent

	hs, ok := parent.(HeaderedSlicer)
	if !ok {
		return nil // nothing to check it against
	}
	if got, want := hs.Header().Metadata.BuildId, b.header.Metadata.BaseBuildId; got != want {
		return fmt.Errorf("block: build %s was diffed against base %s, but build %s "+
			"was attached as its parent", b.buildID, want, got)
	}
	return nil
}

func (b *RemoteBuild) Header() *Header  { return b.header }
func (b *RemoteBuild) BlockSize() int64 { return int64(b.header.Metadata.BlockSize) }
func (b *RemoteBuild) Size() int64      { return int64(b.header.Metadata.Size) }

func (b *RemoteBuild) Close() error {
	if b.data != nil {
		return b.data.Close()
	}
	return nil
}

// Slice serves a logical range, fetching what is missing.
func (b *RemoteBuild) Slice(ctx context.Context, off, length int64) ([]byte, error) {
	storageOff, mappedLen, buildID, err := b.header.GetShiftedMapping(uint64(off))
	if err != nil {
		return nil, err
	}
	if length > int64(mappedLen) {
		length = int64(mappedLen)
	}

	if buildID == nil {
		// A gap: elided because it was all zeros.
		return make([]byte, length), nil
	}

	if *buildID != b.buildID {
		// Stored by the parent because it was identical there.
		//
		// The parent is asked for the SAME LOGICAL offset, not the mapping's
		// storage offset: the whole invariant of a diff is that at a given
		// logical offset the parent already returns the right bytes.
		if b.parent == nil {
			return nil, fmt.Errorf("block: build %s maps offset %d to parent %s, "+
				"but no parent is attached", b.buildID, off, *buildID)
		}
		return b.parent.Slice(ctx, off, length)
	}

	if err := b.ensureCached(ctx, storageOff, uint64(length)); err != nil {
		return nil, err
	}

	out := make([]byte, length)
	n, err := b.data.ReadAt(out, int64(storageOff))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("block: read cached data at %d: %w", storageOff, err)
	}
	// A short read is a legitimately elided tail, which reads back as zeros.
	for i := n; i < len(out); i++ {
		out[i] = 0
	}
	return out, nil
}

// ensureCached fetches any storage blocks for a range that are not local yet.
func (b *RemoteBuild) ensureCached(ctx context.Context, storageOff, length uint64) error {
	blockSize := b.BlockSize()
	first := BlockIdx(int64(storageOff), blockSize)
	last := BlockCeilIdx(int64(storageOff+length), blockSize)

	b.mu.Lock()
	missing := roaring.New()
	for idx := first; idx < last; idx++ {
		if !b.cached.Contains(uint32(idx)) {
			missing.Add(uint32(idx))
		}
	}
	b.mu.Unlock()

	if missing.IsEmpty() {
		return nil
	}
	// Contiguous missing blocks become ONE request. Fetching per block turns a
	// sequential read into thousands of round trips.
	for _, r := range BitsetRanges(missing) {
		if err := b.fetchRange(ctx, r.Start, r.End); err != nil {
			return err
		}
	}
	return nil
}

// fetchRange downloads a run of storage blocks into the local cache.
func (b *RemoteBuild) fetchRange(ctx context.Context, firstBlock, lastBlock int64) error {
	blockSize := b.BlockSize()
	off := firstBlock * blockSize
	length := (lastBlock - firstBlock) * blockSize
	if off+length > b.packedSize {
		length = b.packedSize - off
	}
	if length <= 0 {
		b.markCached(firstBlock, lastBlock)
		return nil
	}

	data, err := b.store.GetRange(ctx, b.buildID.String()+"/data", off, length)
	if err != nil {
		if errors.Is(err, ErrRangeNotSatisfiable) {
			// The data object is shorter than the header implies, which is
			// what a fully-elided build looks like. The cache file is
			// pre-truncated, so those blocks already read back as zeros --
			// mark them present and carry on. Failing here instead kills the
			// wake on a device-sizing timeout, with nothing pointing at the
			// cause.
			b.markCached(firstBlock, lastBlock)
			return nil
		}
		return fmt.Errorf("block: fetch %s data [%d,%d): %w", b.buildID, off, off+length, err)
	}

	if _, err := b.data.WriteAt(data, off); err != nil {
		return fmt.Errorf("block: write cached data at %d: %w", off, err)
	}
	b.markCached(firstBlock, lastBlock)
	return nil
}

func (b *RemoteBuild) markCached(firstBlock, lastBlock int64) {
	b.mu.Lock()
	b.cached.AddRange(uint64(firstBlock), uint64(lastBlock))

	total := TotalBlocks(b.packedSize, b.BlockSize())
	newlyComplete := !b.complete && total > 0 && b.cached.GetCardinality() >= uint64(total)
	if newlyComplete {
		b.complete = true
	}
	b.mu.Unlock()

	if !newlyComplete {
		return
	}
	// Best effort: a missing marker only costs the next open a re-download,
	// which is slow rather than wrong. Writing one that is not true is the
	// failure that matters, so it goes in only once every block is on disk.
	f, err := os.Create(filepath.Join(b.dir, completeMarker))
	if err != nil {
		return
	}
	_ = f.Close()
}

// Prefault pulls the whole packed data file in one request.
//
// This is the anti-fault-storm guarantee. Without it every page fault is its
// own round trip to object storage, which for a few hundred megabytes of guest
// memory means an hour of paging instead of a few seconds. It recurses into
// the parent first, because a diff's unchanged ranges are served from there.
func (b *RemoteBuild) Prefault(ctx context.Context) error {
	if b.parent != nil {
		if err := b.parent.Prefault(ctx); err != nil {
			return err
		}
	}
	if b.packedSize == 0 {
		return nil
	}

	total := TotalBlocks(b.packedSize, b.BlockSize())
	b.mu.Lock()
	complete := b.cached.GetCardinality() >= uint64(total)
	b.mu.Unlock()
	if complete {
		return nil
	}

	return b.fetchRange(ctx, 0, total)
}
