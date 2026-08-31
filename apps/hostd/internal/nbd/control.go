package nbd

import (
	"bytes"
	"fmt"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/ctlsock"
)

// The handler runs as its own process, so hostd can be restarted -- or crash --
// without wedging every running VM's disk. That isolation costs one thing: the
// dirty-block bitmap lives in the child's memory, and a checkpoint needs it in
// the parent's.
//
// Deriving it from the file instead is not an option. The cache is sparse, but
// filesystems allocate in extents larger than a block, so SEEK_DATA reports
// blocks that were never written. Recording one of those as "mine" writes
// zeros over the template's content, which is corruption a test would only
// catch by reading the exact wrong block.
const (
	cmdDirty = "dirty"
	cmdStop  = "stop"
)

// control answers hostd's requests inside the handler process.
func control(cache *block.Cache, stop func()) ctlsock.Handler {
	return func(cmd string) ([]byte, error) {
		switch cmd {
		case cmdDirty:
			// Sync first. The bitmap describes blocks the mapping holds, and a
			// parent that chunkifies the file before those pages reach it
			// would store zeros for blocks the bitmap swears are written --
			// the same corruption the bitmap exists to prevent, arrived at
			// from the other side.
			if err := cache.Sync(); err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			if _, err := cache.Dirty().WriteTo(&buf); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil

		case cmdStop:
			stop()
			return nil, nil

		default:
			return nil, fmt.Errorf("unknown command %q", cmd)
		}
	}
}

// parseDirty decodes a bitmap from the wire.
func parseDirty(payload []byte) (*roaring.Bitmap, error) {
	bm := roaring.New()
	if len(payload) == 0 {
		return bm, nil
	}
	if _, err := bm.ReadFrom(bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("nbd: decode dirty bitmap: %w", err)
	}
	return bm, nil
}
