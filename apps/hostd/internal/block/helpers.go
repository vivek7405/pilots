package block

import "github.com/RoaringBitmap/roaring/v2"

// Block arithmetic. A build is addressed in whole blocks, so these convert
// between byte offsets and block indices consistently everywhere.

// TotalBlocks is how many blocks cover size bytes.
func TotalBlocks(size, blockSize int64) int64 {
	if blockSize <= 0 {
		return 0
	}
	return (size + blockSize - 1) / blockSize
}

// BlockIdx is the index of the block containing off.
func BlockIdx(off, blockSize int64) int64 { return off / blockSize }

// BlockCeilIdx is the index one past the block containing the last byte before
// off -- i.e. the exclusive end of a range starting at 0.
func BlockCeilIdx(off, blockSize int64) int64 {
	return (off + blockSize - 1) / blockSize
}

// BlockOffset is the byte offset where a block starts.
func BlockOffset(idx, blockSize int64) int64 { return idx * blockSize }

// Range is a half-open span of block indices.
type Range struct{ Start, End int64 }

// BitsetRanges collapses a set of block indices into contiguous runs, so a
// caller can act on a few large ranges rather than thousands of single blocks.
func BitsetRanges(b *roaring.Bitmap) []Range {
	if b == nil || b.IsEmpty() {
		return nil
	}

	var out []Range
	cur := Range{Start: -1}

	it := b.Iterator()
	for it.HasNext() {
		idx := int64(it.Next())
		switch {
		case cur.Start < 0:
			cur = Range{Start: idx, End: idx + 1}
		case idx == cur.End:
			cur.End = idx + 1
		default:
			out = append(out, cur)
			cur = Range{Start: idx, End: idx + 1}
		}
	}
	if cur.Start >= 0 {
		out = append(out, cur)
	}
	return out
}
