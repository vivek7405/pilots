package fc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Durability marker files.
//
// A checkpoint returns to the caller as soon as the guest is running again,
// while the upload continues in the background. These files are how a caller
// asks whether the data is actually safe yet.
const (
	durableMarker = ".durable"
	failedMarker  = ".failed"
)

// CheckpointStatus reports where a checkpoint's data currently lives.
type CheckpointStatus struct {
	// Durable is true once the upload has completed: the checkpoint can be
	// restored from any host in the fleet.
	Durable bool `json:"durable"`
	// Present is true while the local copy still exists. A checkpoint that is
	// durable but not present is the normal steady state -- and is what a
	// restore on a DIFFERENT host sees.
	Present bool `json:"present"`
	// Failed is set when the background upload gave up.
	Failed bool   `json:"failed"`
	Error  string `json:"error,omitempty"`
}

// uploadSlots bounds concurrent background uploads.
//
// One by default, and that is not timidity: each upload reads a
// hundreds-of-megabytes image while the host is also serving live machines.
// Running them unbounded was observed to exhaust memory and starve concurrent
// restores. Widening this is a tuning decision to make with measurements.
var uploadSlots = make(chan struct{}, 1)

// Checkpoint captures a restorable point and resumes the guest immediately.
//
// The guest is frozen only for the snapshot write and a reflink copy of its
// disk -- on a filesystem with reflink support that copy is a metadata
// operation, so the pause is short and roughly independent of disk size. The
// upload happens afterwards, with the machine already serving again.
//
// If anything fails between the pause and the resume, the guest is resumed
// anyway. A machine left frozen is worse than a failed checkpoint.
func (m *Machine) Checkpoint(ctx context.Context, up Uploader, at Artifacts, localDir string) (err error) {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fmt.Errorf("fc: mkdir checkpoint dir: %w", err)
	}
	// Clear stale markers: this directory may be a retry of a failed attempt.
	_ = os.Remove(filepath.Join(localDir, durableMarker))
	_ = os.Remove(filepath.Join(localDir, failedMarker))

	p, err := m.pauseAndSnapshot(ctx)
	if err != nil {
		// pauseAndSnapshot may have paused before failing.
		if rerr := m.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
			slog.Error("checkpoint failed and the guest could not be resumed",
				"machine", m.ID, "err", rerr)
		}
		return err
	}

	// Resume as soon as the local copies exist. Everything after this point
	// happens with the machine already running.
	defer func() {
		if rerr := m.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
			slog.Error("guest could not be resumed after checkpoint",
				"machine", m.ID, "err", rerr)
			if err == nil {
				err = fmt.Errorf("fc: resume after checkpoint: %w", rerr)
			}
		}
	}()

	if err := syncRootfs(p.hostRootfs); err != nil {
		return err
	}

	localSnap := filepath.Join(localDir, SnapFile)
	localMem := filepath.Join(localDir, MemFile)
	localRootfs := filepath.Join(localDir, RootfsFile)

	if err := reflinkCopy(p.hostSnap, localSnap); err != nil {
		return err
	}
	if err := reflinkCopy(p.hostMem, localMem); err != nil {
		return err
	}

	// Skip the disk entirely when the machine never wrote to it: the copy
	// would be all zeros and the restore falls back to the template.
	rootfsSaved, err := hasAllocatedBlocks(p.hostRootfs)
	if err != nil {
		return err
	}
	if rootfsSaved {
		if err := reflinkCopy(p.hostRootfs, localRootfs); err != nil {
			return err
		}
	}

	// Upload detached from the request: the caller gets its checkpoint id back
	// now, and asks about durability separately.
	go m.uploadCheckpoint(up, at, localDir, rootfsSaved)
	return nil
}

func (m *Machine) uploadCheckpoint(up Uploader, at Artifacts, localDir string, rootfsSaved bool) {
	uploadSlots <- struct{}{}
	defer func() { <-uploadSlots }()

	// Detached from any request context on purpose: a client that hangs up
	// must not abandon a half-uploaded checkpoint.
	ctx := context.Background()

	uploads := []struct{ key, path string }{
		{at.Snap(), filepath.Join(localDir, SnapFile)},
		{at.Mem(), filepath.Join(localDir, MemFile)},
	}
	if rootfsSaved {
		uploads = append(uploads, struct{ key, path string }{
			at.Rootfs(), filepath.Join(localDir, RootfsFile)})
	}

	for _, u := range uploads {
		if err := up.PutFile(ctx, u.key, u.path); err != nil {
			slog.Error("checkpoint upload failed", "machine", m.ID, "key", u.key, "err", err)
			_ = os.WriteFile(filepath.Join(localDir, failedMarker), []byte(err.Error()), 0o644)
			return
		}
	}

	if err := os.WriteFile(filepath.Join(localDir, durableMarker), nil, 0o644); err != nil {
		slog.Error("could not mark checkpoint durable", "machine", m.ID, "err", err)
	}
}

// StatusOf reports a checkpoint's durability.
//
// Durable-but-not-present is the normal steady state, and is also exactly what
// a restore on another host sees -- so callers must not treat a missing local
// copy as a missing checkpoint.
func StatusOf(localDir string) CheckpointStatus {
	var st CheckpointStatus

	if _, err := os.Stat(filepath.Join(localDir, durableMarker)); err == nil {
		st.Durable = true
	}
	if raw, err := os.ReadFile(filepath.Join(localDir, failedMarker)); err == nil {
		st.Failed = true
		st.Error = string(raw)
	}
	if _, err := os.Stat(filepath.Join(localDir, SnapFile)); err == nil {
		st.Present = true
	}
	return st
}
