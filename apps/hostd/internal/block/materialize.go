package block

import (
	"context"
	"fmt"
	"os"
)

// Materialize writes a build back out as a plain image file.
//
// The engine normally never needs this: a machine reads its disk lazily
// through the block server, one range at a time, which is what makes a restore
// cost the writes rather than the disk. A machine BOOTED from a build is the
// exception -- Firecracker opens a file or a device before the kernel starts,
// there is no snapshot to lazily fault against, and a build produced by
// `POST /v1/builds` has never existed as a file anywhere.
//
// The result is written sparsely: a run of zeros is skipped rather than
// written, so an image whose apparent size is gigabytes costs what its
// contents cost. That matters because this file is then reflink-copied per
// machine, and a reflink of an allocated extent is not free the way a reflink
// of a hole is.
//
// Written to a temporary file and renamed. A half-written image left under the
// final name would be indistinguishable from a complete one on the next start,
// and it boots into a kernel panic rather than an error.
func Materialize(ctx context.Context, src Slicer, dest string) (err error) {
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("block: create %s: %w", tmp, err)
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(tmp)
		}
	}()

	size := src.Size()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("block: size %s: %w", tmp, err)
	}

	// A megabyte at a time: large enough that the per-call overhead disappears
	// and small enough that materialising a large image does not hold a large
	// image in memory.
	// Advance by what Slice RETURNED, never by what it was asked for.
	//
	// A Slicer clamps its answer to the mapping the offset lands in, so a read
	// spanning a mapping boundary comes back short -- which is most reads on
	// any real image, because zero elision cuts a disk into many mappings.
	// Advancing by the request instead skipped every byte between the end of
	// the short read and the end of the window, so everything after the first
	// gap was simply absent from the output. The file still had the right
	// length and its first megabyte was right, so it looked plausible: a built
	// image panicked the guest kernel at mount_block_root with a corrupt
	// journal inode, which names nothing about offsets.
	const chunk = 1 << 20
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := int64(chunk)
		if off+length > size {
			length = size - off
		}
		buf, err := src.Slice(ctx, off, length)
		if err != nil {
			return fmt.Errorf("block: read %d+%d: %w", off, length, err)
		}
		if len(buf) == 0 {
			// No progress is possible from here, and looping would spin
			// forever on a truncated or malformed header.
			return fmt.Errorf("block: read %d+%d returned nothing", off, length)
		}
		if !allZeroBytes(buf) {
			if _, err := f.WriteAt(buf, off); err != nil {
				return fmt.Errorf("block: write %d: %w", off, err)
			}
		}
		// A zero run is left as a hole: the file was truncated to full size,
		// so the range reads back as zeros either way.
		off += int64(len(buf))
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("block: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("block: close %s: %w", tmp, err)
	}
	return os.Rename(tmp, dest)
}

func allZeroBytes(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
