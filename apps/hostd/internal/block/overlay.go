package block

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// Overlay is a copy-on-write view: a writable cache in front of a read-only
// template.
//
// This is how a machine gets its own disk without copying one. Blocks it has
// written come from the cache; everything else falls through to the shared
// template, so a hundred machines from one template cost one template plus
// their individual writes.
type Overlay struct {
	template Slicer
	cache    *Cache
	ejected  atomic.Bool
}

func NewOverlay(template Slicer, cache *Cache) *Overlay {
	return &Overlay{template: template, cache: cache}
}

func (o *Overlay) BlockSize() int64 { return o.template.BlockSize() }
func (o *Overlay) Size() int64      { return o.template.Size() }

func (o *Overlay) Prefault(ctx context.Context) error { return o.template.Prefault(ctx) }

// ReadAt serves a range, block by block.
//
// Per block, not per request: a four-block read can be two blocks the machine
// has written and two it has not, and each half has to come from a different
// place.
func (o *Overlay) ReadAt(p []byte, off int64) (int, error) {
	blockSize := o.BlockSize()
	filled := 0

	for filled < len(p) {
		blockStart := (off + int64(filled)) / blockSize * blockSize
		within := (off + int64(filled)) - blockStart
		want := len(p) - filled
		if avail := int(blockSize - within); want > avail {
			want = avail
		}

		dst := p[filled : filled+want]
		if _, err := o.cache.ReadAt(dst, off+int64(filled)); err == nil {
			metrics.NBDCacheHits.Inc()
			filled += want
			continue
		} else if !errors.Is(err, ErrBytesNotAvailable) && !errors.Is(err, ErrOutOfRange) {
			return filled, err
		}

		// Not written here: read through to the template, looping because a
		// Slice may return less than asked and zero-filling past its end.
		metrics.NBDCacheMisses.Inc()
		got := 0
		for got < want {
			chunk, err := o.template.Slice(context.Background(), off+int64(filled)+int64(got), int64(want-got))
			if err != nil {
				if errors.Is(err, ErrOutOfRange) {
					// Past the template: the rest is zeros.
					for i := got; i < want; i++ {
						dst[i] = 0
					}
					got = want
					break
				}
				return filled, err
			}
			if len(chunk) == 0 {
				for i := got; i < want; i++ {
					dst[i] = 0
				}
				break
			}
			copy(dst[got:], chunk)
			got += len(chunk)
		}
		filled += want
	}
	return filled, nil
}

// WriteAt goes only to the cache. The template is shared and immutable.
func (o *Overlay) WriteAt(p []byte, off int64) (int, error) {
	return o.cache.WriteAt(p, off)
}

// EjectCache hands the cache to the caller and makes Close a no-op.
//
// One-shot, because the point is to take the cache out of the overlay's
// ownership before the overlay is torn down -- the cache file holds writes
// that still have to be chunkified, and Cache.Close deletes it.
func (o *Overlay) EjectCache() (*Cache, error) {
	if !o.ejected.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("block: cache already ejected")
	}
	return o.cache, nil
}

// Close releases the cache unless it has been ejected.
func (o *Overlay) Close() error {
	if o.ejected.Load() {
		return nil
	}
	return o.cache.Close()
}
