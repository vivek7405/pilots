package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// How big a service's replicas are, and how that changes without downtime.
//
// # Why a service has a size at all
//
// A machine's size used to be settled at create and never move, so "give this
// service more memory" meant destroying it and making another under a new URL.
// The size lives on the SERVICE rather than on each replica because replicas
// are cattle: the autoscaler makes one at 3am, self-heal makes one when a host
// dies, a rollout makes three. Each of those has to produce a machine of the
// size the service was last scaled to, and the only way that stays true is for
// there to be one place the size is written down.
//
// # Why scaling is a rollout and not a loop of resizes
//
// Resizing a replica in place takes it down while it boots, because a memory
// image cannot be loaded into a differently-sized VM. Doing that to every
// replica in turn would work and would be needlessly visible: the fleet
// already knows how to bring up a new replica, prove it serves, and retire the
// old one, and that is a rollout. So a scale IS a rollout, of the same release
// the service is already running -- the release id does not change, nothing is
// rebuilt, and no request is dropped.
//
// A volume-backed service is the exception, because a volume has one writer by
// construction: the replacement cannot mount the volume until the old machine
// has let go, so there is a window. The router holds requests across it rather
// than refusing them, which is the same thing it does for a wake.

// sizeOf is the service's size row, or a nil ServiceSize when it has none.
//
// A nil reads as the defaults through ServiceSize.Size, so a service that
// predates the table needs no backfill -- which matters, because backfilling a
// live cr-sqlite table is the incident ARCHITECTURE.md rule 6 exists to
// prevent.
//
// A store error is returned rather than read as "no row": that answer would
// silently create the replica at the default size, and a service scaled to
// 8 GiB would quietly come back at 512 MiB and be killed by its own workload.
func (m *Manager) sizeOf(ctx context.Context, serviceID string) (*state.ServiceSize, error) {
	sz, err := m.opts.Store.GetServiceSize(ctx, serviceID)
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("services: the size %s runs at: %w", serviceID, err)
	}
	return sz, nil
}

// stampImageSize records the size the release's memory image was just
// photographed at.
//
// Called after a snapshot, and only after a successful one. Until it runs, the
// row says the image was taken at some other size and every replica boots
// instead of restoring: slower, and never wrong. The reverse mistake -- a row
// claiming a match that is not there -- fails inside Firecracker as a corrupt
// snapshot, so the order is deliberate.
func (m *Manager) stampImageSize(ctx context.Context, serviceID string) error {
	sz, err := m.sizeOf(ctx, serviceID)
	if err != nil {
		return err
	}
	vcpus, memMiB := sz.Size()
	row := &state.ServiceSize{
		ServiceID: serviceID, VCPUs: vcpus, MemMiB: memMiB,
		ImageVCPUs: vcpus, ImageMemMiB: memMiB, UpdatedAt: time.Now().Unix(),
	}
	if err := m.opts.Store.PutServiceSize(ctx, row); err != nil {
		return fmt.Errorf("services: record the size %s's image was taken at: %w", serviceID, err)
	}
	return nil
}

// Resize changes how big a service's replicas are, with no downtime.
//
// Zero on a dimension leaves that dimension alone, which is how "give it more
// memory" is said without restating the vCPU count.
//
// The release does not change. What happens is the rollout that a deploy would
// run: a replica comes up at the new size, has to pass the same health gate any
// new replica passes, is photographed so its siblings restore rather than boot,
// and only then are the old replicas retired. A service that cannot bring up a
// replica at the new size therefore keeps serving from the old ones, which is
// the behaviour a scale has to have: the failure mode of "more memory" must
// never be "no service".
func (m *Manager) Resize(ctx context.Context, serviceID string, vcpus, memMiB int) (*state.ServiceSize, error) {
	svc, err := m.opts.Store.GetService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	current, err := m.sizeOf(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	curVCPUs, curMemMiB := current.Size()
	if vcpus <= 0 {
		vcpus = curVCPUs
	}
	if memMiB <= 0 {
		memMiB = curMemMiB
	}
	if vcpus == curVCPUs && memMiB == curMemMiB {
		// Already there. A reconciler re-applying its desired size, or a
		// `scale` run twice, must not roll the whole service for nothing.
		return sizeRow(serviceID, vcpus, memMiB, current), nil
	}

	if err := m.beginRollout(serviceID); err != nil {
		return nil, err
	}
	defer m.endRollout(serviceID)

	var rel *state.Release
	if svc.ReleaseID != "" {
		if rel, err = m.opts.Store.GetRelease(ctx, svc.ReleaseID); err != nil {
			return nil, fmt.Errorf("services: the release %s is running: %w", serviceID, err)
		}
	}

	// The row FIRST, and with the image size left where it was. Every replica
	// created from here on is the new size, and until the new release image is
	// photographed the mismatch makes them boot rather than restore -- which is
	// exactly right, because the image on hand is the old size.
	row := &state.ServiceSize{
		ServiceID: serviceID, VCPUs: vcpus, MemMiB: memMiB,
		UpdatedAt: time.Now().Unix(),
	}
	if current != nil {
		row.ImageVCPUs, row.ImageMemMiB = current.ImageVCPUs, current.ImageMemMiB
	} else {
		row.ImageVCPUs, row.ImageMemMiB = state.DefaultServiceVCPUs, state.DefaultServiceMemMiB
	}
	if err := m.opts.Store.PutServiceSize(ctx, row); err != nil {
		return nil, fmt.Errorf("services: record %s's new size: %w", serviceID, err)
	}

	slog.Info("scaling a service to a new size; its replicas are replaced one at a "+
		"time and no request is dropped", "service", serviceID,
		"from_vcpus", curVCPUs, "to_vcpus", vcpus,
		"from_mem_mib", curMemMiB, "to_mem_mib", memMiB)

	if rel == nil {
		// Nothing is running yet, so the row IS the whole operation: the next
		// deploy creates replicas at this size.
		return row, nil
	}

	volumeID, err := m.volumeOf(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if volumeID != "" {
		if err := m.resizeOnVolume(ctx, svc, rel, volumeID); err != nil {
			return nil, err
		}
		return row, nil
	}

	if err := m.resizeStateless(ctx, svc, rel); err != nil {
		return nil, err
	}
	return row, nil
}

// sizeRow is the row a no-op resize reports, without writing anything.
func sizeRow(serviceID string, vcpus, memMiB int, current *state.ServiceSize) *state.ServiceSize {
	row := &state.ServiceSize{ServiceID: serviceID, VCPUs: vcpus, MemMiB: memMiB}
	if current != nil {
		row.ImageVCPUs, row.ImageMemMiB = current.ImageVCPUs, current.ImageMemMiB
		row.UpdatedAt = current.UpdatedAt
	} else {
		row.ImageVCPUs, row.ImageMemMiB = state.DefaultServiceVCPUs, state.DefaultServiceMemMiB
	}
	return row
}

// resizeStateless replaces every replica at the new size, one at a time.
//
// This is rollOut against the release the service is ALREADY on, with the
// memory image cleared so replica one boots at the new size and is
// re-photographed. Every later replica restores from that new image. The old
// replicas are suspended and pruned exactly as a deploy retires them, so the
// router is serving from the new ones before the old ones stop.
func (m *Manager) resizeStateless(ctx context.Context, svc *state.Service, rel *state.Release) error {
	knobs, err := m.replicaKnobs(ctx, svc, nil)
	if err != nil {
		return err
	}
	health, err := ParseHealth(svc.Health)
	if err != nil {
		return err
	}

	// Captured BEFORE the rollout, and by id. The release does not change
	// here, so the old replicas and the new ones share a release id and
	// nothing in the store tells them apart afterwards -- the list taken now
	// is the only thing that does.
	before, err := m.replicasOf(ctx, svc.ID, rel.ID)
	if err != nil {
		return err
	}
	previous := make([]string, 0, len(before))
	for _, mach := range before {
		previous = append(previous, mach.ID)
	}

	replicas := svc.Replicas
	if replicas < 1 {
		replicas = 1
	}

	// Same release id. A scale is not a deploy: nothing was rebuilt, and a new
	// release row would make the service's history claim a version that does
	// not exist.
	created, err := m.rollOut(ctx, svc, rel, health, replicas, knobs)
	if err != nil {
		// Whatever came up at the new size is wreckage, and the OLD replicas
		// are still serving. Clean up and leave the service where it was.
		m.cleanUp(ctx, svc.ID, created, "a scale that could not bring up a replica at the new size")
		return err
	}

	// The release's memory image is now a photograph of the NEW size, so
	// later replicas restore rather than boot. Best effort, like the snapshot
	// itself: an unrecorded match only costs later replicas a cold boot, while
	// a recorded one that is false fails inside Firecracker.
	if err := m.stampImageSize(ctx, svc.ID); err != nil {
		slog.Warn("could not record the size the release's image was taken at; "+
			"replicas will boot rather than restore", "service", svc.ID, "err", err)
	}

	// DESTROYED, not suspended. A deploy suspends its predecessors because
	// they are a rollback target; these are not -- they carry the same release
	// as the machines now serving it, and waking one would put a replica of
	// the OLD size back into the pool, which is the one thing this operation
	// exists to prevent.
	for _, id := range previous {
		if err := m.opts.Machines.Destroy(ctx, id); err != nil {
			slog.Warn("could not retire a replica of the old size",
				"service", svc.ID, "machine", id, "err", err)
		}
	}
	return nil
}

// resizeOnVolume replaces the ONE machine a volume-backed service runs.
//
// There is no overlap to hide the window behind: a volume has a single writer,
// so the replacement cannot mount it until this machine has let go. The window
// is a redeploy's, and the router holds requests across it the same way it
// holds a request that arrives while a machine is waking, so a request posted
// during a scale is served late rather than refused.
func (m *Manager) resizeOnVolume(ctx context.Context, svc *state.Service,
	rel *state.Release, volumeID string) error {

	mach, err := m.machineOf(ctx, svc.ID)
	if err != nil {
		return err
	}
	if mach == nil {
		return nil
	}
	// The redeploy path, onto the SAME release: it kills the process, writes
	// the row, and boots from the rootfs, and machineFCConfig reads the size
	// off the row it just wrote. Nothing about the volume moves.
	if err := m.redeploy(ctx, mach, rel); err != nil {
		return fmt.Errorf("services: scale %s on its volume: %w", svc.ID, err)
	}
	return nil
}
