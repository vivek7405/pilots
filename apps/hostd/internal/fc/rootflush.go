package fc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"

	"github.com/pilotsrun/pilots/hostd/internal/block"
	"github.com/pilotsrun/pilots/hostd/internal/metrics"
)

// FlushCowPath is where a machine's flushed disk is staged: a copy of its cow
// that is whole as of the last root flush's pause, and that persists between
// flushes so each one adds only what was written since.
func FlushCowPath(stateDir string) string { return filepath.Join(stateDir, "flush.cow") }

// StageRootFlush freezes the guest just long enough to copy the blocks written
// since the previous flush into the staged cow, and returns the finisher that
// makes them durable in object storage. A nil finisher means there was nothing
// to make durable.
//
// It is the disk half of CheckpointInstant and nothing else: pause, copy,
// resume, then chunkify and upload with the guest already serving. No vmstate,
// no memory image, no makeMemoryResident -- which is what makes it cheap
// enough to run on a timer. The pause is proportional to the bytes written
// since the previous flush, not to the disk and not to everything written
// since the wake: the block server keeps a bitmap of exactly those blocks, and
// they are merged into a staged copy of the cow that persists between flushes,
// so the staged copy is always the whole cow as of the last pause while each
// pause copies only the delta. The first flush after an attach copies the
// whole cow.
//
// It is SPLIT IN TWO on purpose. The pause belongs under whatever lock the
// caller holds over this machine; the chunkify and the upload that follow
// belong under no lock at all. Held together, a background timer would own a
// machine for the length of an upload -- and a deploy's checkpoint, a suspend
// or a destroy arriving meanwhile would queue behind a loop that exists to be
// invisible. What serialises the finisher against another capture is the
// capture gate, held from here until it returns, exactly as CheckpointInstant
// holds it across its own background half.
//
// The build the finisher produces is a diff against the template, exactly like
// a suspend's, never against the previous flush: a chain of flushes would be a
// chain of parents, and the block layer refuses grandparent chains for the
// reason chunkify.go gives. Each build is complete on its own, so the previous
// flush's is superseded the moment the row names this one. The finisher
// returns only once the build is uploaded: what the caller does with the id is
// name it on the row, and a row naming a build that is not in object storage
// is a rescue that fails on another host.
func (m *Machine) StageRootFlush(ctx context.Context) (
	func(context.Context, Uploader, SnapshotOpts) (uuid.UUID, error), error) {

	if m.NBD == nil {
		// A file-backed disk has no block server and no bitmap. Only the
		// throwaway template machine is in that state now, and its disk is
		// captured by the template build, not by a flush.
		return nil, nil
	}
	// Never overlapping a checkpoint or a suspend, in either direction: they
	// read the same bitmap and stage from the same cow, and the capture gate
	// is what serialises them. Never WAITED for, either: this runs under the
	// machine's lock, and a checkpoint's upload runs with that lock free, so
	// a tick landing inside the upload would have held the lock for its
	// whole length and queued a suspend, a destroy or a rollout behind a
	// background loop -- exactly what the lock's TryLock exists to prevent,
	// through a door the TryLock could not see. A capture in flight is a
	// disk already being made durable; the next tick has nothing to add.
	if m.CaptureInFlight() {
		return nil, nil
	}
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
		return nil, fmt.Errorf("fc: root flush of %s: %w", m.ID, err)
	}
	if dirty == nil {
		return nil, nil
	}
	metrics.RootFlushPauseSeconds.Observe(pause.Seconds())

	// Everything from here runs with the guest serving. A suspend or a
	// checkpoint that arrives meanwhile takes the machine's lock at once and
	// waits at awaitCapture, which is where waiting belongs.
	m.beginCapture()
	return func(ctx context.Context, chunks Uploader, opts SnapshotOpts) (uuid.UUID, error) {
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

		// The realised window: how long the oldest write this flush made
		// durable had been at risk. Measured from the previous durable instant
		// to the pause above, which is the number the published RPO bounds.
		metrics.RootFlushLagSeconds.Observe(pausedAt.Sub(m.lastRootFlush).Seconds())
		m.lastRootFlush = pausedAt
		return build, nil
	}, nil
}
