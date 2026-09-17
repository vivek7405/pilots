package block

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/edsrzf/mmap-go"
	"golang.org/x/sys/unix"
)

// Cache is a machine's copy-on-write layer.
//
// Backed by a sparse file: it is truncated to the full logical size but
// allocates nothing until written, so a machine that touches a little of its
// disk costs a little disk. A roaring bitmap tracks which blocks have actually
// been written, which is what distinguishes "written zeros" from "never
// written" -- the two are identical on disk and mean opposite things.
type Cache struct {
	mu        sync.RWMutex
	size      int64
	blockSize int64
	path      string

	file *os.File
	data mmap.MMap

	// dirty holds the indices of blocks this cache owns. A block not in here
	// must be served by whatever sits underneath.
	dirty *roaring.Bitmap
	// unflushed holds the blocks written since the last acknowledged root
	// flush -- a subset of dirty, cleared by MarkFlushed. It is what keeps a
	// periodic flush's pause proportional to what was written since the
	// previous one rather than to everything written since the wake.
	unflushed *roaring.Bitmap
	// pendingFlush is what the last Unflushed call handed out, so that
	// MarkFlushed clears exactly that and nothing written afterwards.
	pendingFlush *roaring.Bitmap
}

// NewCache opens or creates a cache file.
//
// dirtyFile marks every block as already owned, for a cache that is a complete
// materialization rather than an overlay. The rootfs CoW path passes false:
// its whole purpose is to fall through to the template for blocks it has not
// written.
func NewCache(size, blockSize int64, path string, dirtyFile bool) (*Cache, error) {
	c := &Cache{
		size: size, blockSize: blockSize, path: path,
		dirty: roaring.New(), unflushed: roaring.New(),
	}

	// A zero-size cache is legal -- a machine with no disk of its own -- and
	// every operation short-circuits rather than mapping an empty region.
	if size == 0 {
		return c, nil
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("block: open cache %s: %w", path, err)
	}
	// Sparse: this reserves the address range without allocating blocks.
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, fmt.Errorf("block: truncate cache %s: %w", path, err)
	}

	data, err := mmap.MapRegion(f, int(size), mmap.RDWR, 0, 0)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("block: mmap cache %s: %w", path, err)
	}

	c.file, c.data = f, data
	if dirtyFile {
		c.dirty.AddRange(0, uint64(TotalBlocks(size, blockSize)))
	}
	return c, nil
}

func (c *Cache) Size() int64      { return c.size }
func (c *Cache) BlockSize() int64 { return c.blockSize }
func (c *Cache) Path() string     { return c.path }

// ReadAt serves a range only if EVERY block covering it has been written.
//
// Partial ownership is a miss, not a partial read: the caller needs one answer
// per range, and a half-served range would silently mix this cache's zeros
// with the template's real content.
func (c *Cache) ReadAt(p []byte, off int64) (int, error) {
	if c.data == nil {
		return 0, ErrBytesNotAvailable
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	if off < 0 || off >= c.size {
		return 0, fmt.Errorf("%w: offset %d outside cache of %d", ErrOutOfRange, off, c.size)
	}
	end := off + int64(len(p))
	if end > c.size {
		end = c.size
	}

	first := uint64(BlockIdx(off, c.blockSize))
	last := uint64(BlockCeilIdx(end, c.blockSize))

	// roaring v2 has no ContainsRange. A span here is a handful of blocks at
	// most -- the NBD path asks a block at a time -- so this walks them rather
	// than allocating a second bitmap per read on the hot I/O path.
	if c.dirty.IsEmpty() {
		return 0, ErrBytesNotAvailable
	}
	for idx := first; idx < last; idx++ {
		if !c.dirty.Contains(uint32(idx)) {
			return 0, ErrBytesNotAvailable
		}
	}

	n := copy(p, c.data[off:end])
	return n, nil
}

// WriteAt records a range as owned by this cache.
func (c *Cache) WriteAt(p []byte, off int64) (int, error) {
	if c.data == nil {
		return 0, fmt.Errorf("block: write to a zero-size cache")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if off < 0 || off+int64(len(p)) > c.size {
		return 0, fmt.Errorf("%w: write of %d bytes at %d exceeds cache of %d",
			ErrOutOfRange, len(p), off, c.size)
	}

	n := copy(c.data[off:], p)
	first, last := uint64(BlockIdx(off, c.blockSize)), uint64(BlockCeilIdx(off+int64(n), c.blockSize))
	c.dirty.AddRange(first, last)
	c.unflushed.AddRange(first, last)
	return n, nil
}

// Dirty returns a copy of the owned-block set.
func (c *Cache) Dirty() *roaring.Bitmap {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dirty.Clone()
}

// Unflushed returns a copy of the blocks written since the last MarkFlushed,
// and remembers the set it handed out so MarkFlushed can clear exactly that.
//
// The caller is expected to hold the guest paused from this call through
// MarkFlushed: then nothing is written in between, the remembered set is the
// whole unflushed set, and a caller that copies what it was given and then
// acknowledges has lost nothing.
func (c *Cache) Unflushed() *roaring.Bitmap {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingFlush = c.unflushed.Clone()
	return c.unflushed.Clone()
}

// MarkFlushed forgets the blocks the last Unflushed handed out. A block
// written since then stays unflushed, which is why this clears the remembered
// set rather than everything.
func (c *Cache) MarkFlushed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingFlush != nil {
		c.unflushed.AndNot(c.pendingFlush)
		c.pendingFlush = nil
	}
}

// Sync flushes the mapping to the backing file.
func (c *Cache) Sync() error {
	if c.data == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.Flush()
}

// Close unmaps and REMOVES the backing file.
//
// The removal is why the NBD handler must never close its cache on exit: the
// file holds the machine's un-chunkified writes, and hostd chunkifies it after
// the handler is gone.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	if c.data != nil {
		if err := c.data.Unmap(); err != nil {
			errs = append(errs, err)
		}
		c.data = nil
	}
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			errs = append(errs, err)
		}
		c.file = nil
	}
	if c.path != "" {
		if err := os.RemoveAll(c.path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ExportToDiff copies the owned blocks into a standalone file, returning the
// set that was written.
//
// Used at suspend: the result is chunkified into a build while the cache
// itself is discarded. The copy is CopyDirtyRanges over this cache's own
// file, so it costs what the machine wrote, not what its disk is sized.
func (c *Cache) ExportToDiff(dstPath string) (*roaring.Bitmap, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.data == nil {
		return roaring.New(), nil
	}
	// Best effort: get the mapping's writes into the file before copying it.
	if c.file != nil {
		_ = unix.SyncFileRange(int(c.file.Fd()), 0, 0, unix.SYNC_FILE_RANGE_WRITE)
	}
	if err := CopyDirtyRanges(c.path, dstPath, c.dirty, c.blockSize); err != nil {
		return nil, err
	}
	return c.dirty.Clone(), nil
}

// CopyDirtyRanges copies the blocks in dirty from srcPath into a fresh sparse
// file at dstPath of the same apparent size, at the same offsets.
//
// Only the dirty ranges are read, so the cost is proportional to what the
// machine has written, not to the size of its disk, on every filesystem. That
// is what lets a checkpoint stage its cow inside the pause without a
// filesystem that can share extents: a reflink of the whole cow was free
// where extents are shared and a full copy of the disk everywhere else, and
// the pause must not depend on which of the two the host has.
func CopyDirtyRanges(srcPath, dstPath string, dirty *roaring.Bitmap, blockSize int64) error {
	return copyDirtyRanges(srcPath, dstPath, dirty, blockSize, os.O_TRUNC)
}

// MergeDirtyRanges is CopyDirtyRanges into a file that is kept: the ranges
// land over whatever dstPath already holds, and everything else in it stays.
//
// For a staged copy that persists between root flushes. Each flush merges
// only the blocks written since the previous one, so the file is always the
// whole cow as of the last pause while each pause costs only the delta.
func MergeDirtyRanges(srcPath, dstPath string, dirty *roaring.Bitmap, blockSize int64) error {
	return copyDirtyRanges(srcPath, dstPath, dirty, blockSize, 0)
}

func copyDirtyRanges(srcPath, dstPath string, dirty *roaring.Bitmap, blockSize int64, truncate int) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("block: open %s: %w", srcPath, err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return fmt.Errorf("block: stat %s: %w", srcPath, err)
	}
	size := st.Size()

	dst, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|truncate, 0o600)
	if err != nil {
		return fmt.Errorf("block: create %s: %w", dstPath, err)
	}
	defer dst.Close()
	// Sparse: the same apparent size as the source, allocating nothing until
	// a dirty range lands.
	if err := dst.Truncate(size); err != nil {
		return fmt.Errorf("block: truncate %s: %w", dstPath, err)
	}

	// CopyFileRange lets the kernel share extents where it can. Once it fails
	// for a structural reason -- different filesystems, unsupported -- it will
	// fail for every subsequent range too, so the fallback is sticky rather
	// than retried per range.
	useCopyFileRange := true
	var buf []byte

	for _, r := range BitsetRanges(dirty) {
		off := r.Start * blockSize
		length := (r.End - r.Start) * blockSize
		if off+length > size {
			length = size - off
		}

		if useCopyFileRange {
			if err := copyFileRange(src, dst, off, length); err != nil {
				if !isUnsupported(err) {
					return err
				}
				useCopyFileRange = false
			} else {
				continue
			}
		}
		if buf == nil {
			buf = make([]byte, copyBufSize)
		}
		for done := int64(0); done < length; {
			n := min(int64(len(buf)), length-done)
			if _, err := src.ReadAt(buf[:n], off+done); err != nil {
				return fmt.Errorf("block: read range at %d: %w", off+done, err)
			}
			if _, err := dst.WriteAt(buf[:n], off+done); err != nil {
				return fmt.Errorf("block: copy range at %d: %w", off+done, err)
			}
			done += n
		}
	}
	return nil
}

// copyBufSize bounds the fallback copy's buffer when the kernel cannot copy
// a range for us: one dirty run can be most of a disk.
const copyBufSize = 4 << 20

// PopulateFromSlicer replays a build into this cache.
//
// Used on wake: the machine's previous writes are restored so the guest sees
// them on its first read.
//
// Mappings pointing at ANOTHER build are skipped deliberately. Such a range
// means "unchanged, read it from the template at runtime" -- marking it owned
// here would make this cache's zeros shadow the template's real content, which
// is silent disk corruption with no error anywhere.
func (c *Cache) PopulateFromSlicer(ctx context.Context, src HeaderedSlicer) error {
	if c.data == nil {
		return nil
	}
	header := src.Header()

	for _, m := range header.Mapping {
		if m.BuildId != header.Metadata.BuildId {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		off := int64(m.Offset)
		remaining := int64(m.Length)
		for remaining > 0 {
			chunk, err := src.Slice(ctx, off, remaining)
			if err != nil {
				return fmt.Errorf("block: rehydrate at %d: %w", off, err)
			}
			if len(chunk) == 0 {
				return fmt.Errorf("block: rehydrate stalled at %d", off)
			}
			if _, err := c.WriteAt(chunk, off); err != nil {
				return err
			}
			off += int64(len(chunk))
			remaining -= int64(len(chunk))
		}
	}
	return nil
}

func copyFileRange(src, dst *os.File, off, length int64) error {
	remaining := length
	srcOff, dstOff := off, off
	for remaining > 0 {
		n, err := unix.CopyFileRange(int(src.Fd()), &srcOff, int(dst.Fd()), &dstOff, int(remaining), 0)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("block: copy_file_range made no progress at %d", srcOff)
		}
		remaining -= int64(n)
	}
	return nil
}

func isUnsupported(err error) bool {
	return errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOSYS)
}
