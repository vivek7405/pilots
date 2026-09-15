package fc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// FlushCowPath is where a machine's flushed disk is staged: a copy of its cow
// that is whole as of the last root flush's pause, and that persists between
// flushes so each one adds only what was written since.
func FlushCowPath(stateDir string) string { return filepath.Join(stateDir, "flush.cow") }

// FlushRoot makes the machine's disk durable in object storage without
// capturing its memory, and returns the build it produced -- uuid.Nil when
// there was nothing new to make durable.
//
// It is the disk half of CheckpointInstant and nothing else: pause, copy the
// blocks written since the previous flush, resume, then chunkify and upload
// with the guest already serving. No vmstate, no memory image, no
// makeMemoryResident -- which is what makes it cheap enough to run on a
// timer. The pause is proportional to the bytes written since the previous
// flush, not to the disk and not to everything written since the wake: the
// block server keeps a bitmap of exactly those blocks, and they are merged
// into a staged copy of the cow that persists between flushes, so the staged
// copy is always the whole cow as of the last pause while each pause copies
// only the delta. The first flush after an attach copies the whole cow.
//
// The build is a diff against the template, exactly like a suspend's, never
// against the previous flush: a chain of flushes would be a chain of parents,
// and the block layer refuses grandparent chains for the reason chunkify.go
// gives. Each build is complete on its own, so the previous flush's is
// superseded the moment the row names this one.
//
// It returns only once the build is uploaded: what the caller does with the
// id is name it on the row, and a row naming a build that is not in object
// storage is a rescue that fails on another host.
func (m *Machine) FlushRoot(ctx context.Context, chunks Uploader, opts SnapshotOpts) (uuid.UUID, error) {
	if m.NBD == nil {
		// A file-backed disk has no block server and no bitmap. Only the
		// throwaway template machine is in that state now, and its disk is
		// captured by the template build, not by a flush.
		return uuid.Nil, nil
	}
	// Never overlapping a checkpoint or a suspend, in either direction: they
	// read the same bitmap and stage from the same cow, and the capture gate
	// is what serialises them.
	m.awaitCapture()
	if m.lastRootFlush.IsZero() {
		m.lastRootFlush = m.StartedAt
	}

	staged := FlushCowPath(m.StateDir)
	var dirty *roaring.Bitmap
	pausedAt := time.Now()
	err := m.WhilePaused(ctx, func() error {
		// Cumulative, for the chunkify: the build is diffed against the
		// template. Reading it also flushes the device, so a write the guest
		// completed but the host's page cache still held reaches the handler
		// before either bitmap is read.
		var err error
		if dirty, err = m.NBD.Dirty(); err != nil {
			return err
		}
		if dirty.IsEmpty() {
			dirty = nil
			return nil
		}
		delta, err := m.NBD.Unflushed()
		if err != nil {
			return err
		}

		if _, statErr := os.Stat(staged); statErr != nil || !m.rootStaged {
			// The first flush since this attach, or a staged copy that is
			// gone: the whole cow, into a fresh file.
			if err := block.CopyDirtyRanges(CowPath(m.StateDir), staged, dirty, block.DefaultBlockSize); err != nil {
				return err
			}
			m.rootStaged = true
		} else if delta.IsEmpty() {
			if !m.rootFlushOwed {
				// Nothing written since the last flush, and that flush
				// completed: the disk is already durable as it stands.
				dirty = nil
			}
			return nil
		} else if err := block.MergeDirtyRanges(CowPath(m.StateDir), staged, delta, block.DefaultBlockSize); err != nil {
			return err
		}
		// Acknowledged INSIDE the pause. Nothing can be written between the
		// read and this, so what the handler forgets is exactly what was
		// copied -- and a crash before this line forgets nothing, so the next
		// flush copies it again.
		return m.NBD.Flushed()
	})
	pause := time.Since(pausedAt)
	if err != nil {
		return uuid.Nil, fmt.Errorf("fc: root flush of %s: %w", m.ID, err)
	}
	if dirty == nil {
		return uuid.Nil, nil
	}
	metrics.RootFlushPauseSeconds.Observe(pause.Seconds())

	// Everything from here runs with the guest serving. A suspend or a
	// checkpoint that arrives meanwhile waits at awaitCapture.
	m.beginCapture()
	defer m.endCapture()

	build := uuid.New()
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In:        staged,
		OutDir:    filepath.Join(opts.BuildDir, build.String()),
		BuildID:   build,
		ParentDir: opts.RootfsTemplateDir,
		Dirty:     dirty,
	}); err != nil {
		m.rootFlushOwed = true
		return uuid.Nil, fmt.Errorf("fc: chunkify flushed disk of %s: %w", m.ID, err)
	}
	if err := uploadBuild(ctx, chunks, opts.BuildDir, build); err != nil {
		m.rootFlushOwed = true
		return uuid.Nil, fmt.Errorf("fc: upload flushed disk of %s: %w", m.ID, err)
	}
	m.rootFlushOwed = false

	// The realised window: how long the oldest write this flush made durable
	// had been at risk. Measured from the previous durable instant to this
	// pause, which is the number the published RPO is a bound on.
	metrics.RootFlushLagSeconds.Observe(pausedAt.Sub(m.lastRootFlush).Seconds())
	m.lastRootFlush = pausedAt
	return build, nil
}
