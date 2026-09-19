package machines

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pilotsrun/pilots/hostd/internal/api"
	"github.com/pilotsrun/pilots/hostd/internal/fc"
	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// Changing how big a machine is.
//
// # Why it is a boot and not a resume
//
// A guest's vCPU count and memory size are baked into its snapshot: a memory
// image describes a machine of exactly that size, and Firecracker will not
// load one into a differently-sized VM. So a resize cannot be a restore, and
// the machine loses what was in memory. That is the honest cost and it is
// stated here rather than discovered: the disk survives, the volume survives,
// the id, the name, the URL and the token survive, and the processes start
// again.
//
// Fly pays the same cost for the same reason (`fly scale vm` restarts the
// Machine), and no microVM platform avoids it, because the constraint is the
// hypervisor's rather than the platform's.
//
// # What a caller sees
//
// A single machine: a short outage while it boots at the new size, held by
// nothing. A SERVICE with more than one replica: nothing, because the rollout
// resizes them one at a time and the router serves the others meanwhile. A
// service with one replica and a volume: a held window, because the volume is
// mounted once and the replacement cannot mount it until the old one has let
// go.
//
// That last case is why a resize is not simply "create a new machine and
// destroy the old one". A volume has a single writer by construction, so the
// two cannot overlap, and the request that arrives during the window is held
// by the router's wake path rather than refused.

// The bounds one machine is held to. Taken from the API package rather than
// restated, so the resize route and the service's size cannot come to disagree
// about how big a machine may be.
const (
	MaxVCPUs  = api.MaxVCPUs
	MaxMemMiB = api.MaxMemMiB
	MinMemMiB = api.MinMemMiB
)

// Resize boots a machine again at a new size, in place.
//
// Same row, same URL, same disk, same volume. Returns the updated row.
func (m *Manager) Resize(ctx context.Context, id string, vcpus, memMiB int) (*state.Machine, error) {
	// The bounds that need no row: negative, or over the ceiling. The FLOOR
	// waits until the size is resolved; see below.
	if err := validateBounds(vcpus, memMiB); err != nil {
		return nil, err
	}

	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.HostID != m.opts.HostID {
		return nil, fmt.Errorf("machines: %s is held by %s, not this host: %w",
			id, row.HostID, state.ErrNotOwner)
	}
	if vcpus == 0 {
		vcpus = row.VCPUs
	}
	if memMiB == 0 {
		memMiB = row.MemMiB
	}
	if vcpus == row.VCPUs && memMiB == row.MemMiB {
		// Nothing to do, and a no-op must not cost the machine its memory.
		return row, nil
	}

	// The floor is checked HERE, on the size the machine will actually have.
	//
	// It used to be checked before the two zero-fills above, and validateSize
	// returned early for a PARTIAL resize -- one field zero and the other not
	// -- because zero there means "leave this one alone". That early return
	// skipped the floor, so `--mem 32` on a machine with vCPUs was accepted,
	// filled in from the row, and resized to a size no guest can boot. The one
	// request shape the floor exists for was the one shape that never reached
	// it.
	if err := validateSize(vcpus, memMiB); err != nil {
		return nil, err
	}

	// A replica belongs to its service, not to whoever holds its id. Resizing
	// one behind the service's back would leave the fleet serving one request
	// in three from a machine of a different size, and the next rollout would
	// silently put it back, because a replica is created from the service's
	// own size. So the caller is sent to the operation that resizes them all.
	if row.ServiceID != "" {
		return nil, fmt.Errorf("%w: %s is a replica of service %s; resize the "+
			"service instead, with `pilot services scale %s --vcpus N --mem N`, "+
			"so every replica moves together and the next rollout keeps the size",
			api.ErrConflict, id, row.ServiceID, row.ServiceID)
	}

	// A checkpoint describes a machine of the old size and could never be
	// restored into the new one, so a machine that has one is refused rather
	// than quietly given checkpoints that do not work. The same reasoning
	// Redeploy uses, for the same shape of damage.
	if cks, err := m.opts.Store.ListCheckpoints(ctx, id); err != nil {
		return nil, fmt.Errorf("machines: list checkpoints of %s: %w", id, err)
	} else if len(cks) > 0 {
		return nil, fmt.Errorf("%w: %s has %d checkpoint(s) taken at its current "+
			"size; a memory image cannot be restored into a machine of another "+
			"size, so delete them before resizing", api.ErrConflict, id, len(cks))
	}

	slog.Info("resizing a machine; it boots at the new size and loses what was "+
		"in memory", "machine", id,
		"from_vcpus", row.VCPUs, "to_vcpus", vcpus,
		"from_mem_mib", row.MemMiB, "to_mem_mib", memMiB)

	// The disk first, while the guest is still there to flush it. The boot
	// below comes up on the machine's own disk chain, so that chain has to
	// hold every write -- including the ones since its last suspend, which
	// live only in the copy-on-write file of the process about to be killed.
	// This used to boot through bootMachine, which reflink-copies a fresh
	// template or image: the id, the URL and the token survived, and every
	// file the machine had written was gone.
	var supersededDisk string
	if fcm, ok := m.get(id); ok {
		captured, err := m.captureDiskForResize(ctx, row, fcm)
		if err != nil {
			return nil, fmt.Errorf("machines: resize %s: its disk could not be "+
				"captured, so it was left running at its old size: %w", id, err)
		}
		if captured != "" {
			supersededDisk, row.RootfsBuildID = row.RootfsBuildID, captured
		}
	}

	// Down exactly as Redeploy takes it down. Nothing is photographed: a
	// photograph of the old size is a photograph nothing can load.
	m.releaseDiscovery(id)
	if fcm, ok := m.get(id); ok {
		slotIdx := 0
		if fcm.Slot != nil {
			slotIdx = fcm.Slot.Idx
		}
		if err := fcm.Kill(); err != nil {
			return nil, fmt.Errorf("machines: kill %s: %w", id, err)
		}
		m.drop(id)
		if slotIdx > 0 {
			m.pool.Return(slotIdx)
		}
	}
	// The memory image, and ONLY the memory image: it describes a machine of
	// the old size. The disk is what this machine keeps.
	superseded := row.MemBuildID
	row.MemBuildID = ""

	// Storage only while it is down.
	m.opts.Usage.Transition(id, StateCreating)

	row.VCPUs, row.MemMiB = vcpus, memMiB
	row.State = StateCreating
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, withoutSlot(row)); err != nil {
		return nil, err
	}

	// The machine's own disk chain, booted with a kernel at the new size. The
	// agent token is already on that disk, written at create and flushed by
	// the sync above, so there is nothing to install.
	token := m.token(id)
	// Room for the NEW size, checked before anything is brought up. A resize
	// used to boot straight into memory this host may not have -- the failure
	// deep inside Firecracker that admit exists to replace on the create path.
	// The old process is dead by here, so its memory is free to count. A
	// refusal lands in the same error branch as a failed boot: the machine is
	// down either way.
	var fcm *fc.Machine
	release, err := m.admit(ctx, vcpus, memMiB)
	if err == nil {
		defer release()
		fcm, err = m.bootFromDisk(ctx, row, fc.Backends{}, row.RootfsBuildID)
	}
	if err != nil {
		row.State = StateError
		stampSlot(row, nil)
		row.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, row)
		m.opts.Usage.Transition(id, StateError)
		return row, fmt.Errorf("machines: resize %s: %w", id, err)
	}

	m.put(id, fcm)
	m.rememberToken(id, token)

	row.State = StateRunning
	stampSlot(row, fcm)
	row.LastActivity = time.Now().Unix()
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return row, err
	}
	// Metered at the NEW size from here, which is the point of the whole
	// operation as far as an invoice is concerned.
	m.opts.Usage.Open(id, m.orgOf(ctx, id), StateRunning, vcpus, memMiB,
		m.volumeGiB(ctx, row.VolumeID))

	// The old memory image only after the row no longer names it, so a failed
	// write never leaves the row pointing at an object that is gone. It
	// describes a machine of the old size and nothing can ever load it again.
	if supersededDisk != "" {
		m.discardBuilds(ctx, supersededDisk)
	}
	if superseded != "" {
		m.discardBuilds(ctx, superseded)
	}
	return row, nil
}

// validateBounds refuses what needs no row to refuse: a negative size, or one
// over the ceiling. Checked at the API's edge, before any lock or store read.
func validateBounds(vcpus, memMiB int) error {
	if vcpus < 0 || memMiB < 0 {
		return fmt.Errorf("%w: a size cannot be negative", ErrInvalid)
	}
	if vcpus > MaxVCPUs {
		return fmt.Errorf("%w: %d vCPUs is over the %d a machine may have",
			ErrInvalid, vcpus, MaxVCPUs)
	}
	if memMiB > MaxMemMiB {
		return fmt.Errorf("%w: %d MiB is over the %d a machine may have",
			ErrInvalid, memMiB, MaxMemMiB)
	}
	return nil
}

// validateSize refuses a size no host could hold, on a RESOLVED size: both
// fields filled in, which is what the machine will actually be given.
//
// The floor is why it has to be the resolved one. A partial resize names one
// field and leaves the other zero, and zero there means "leave it alone" --
// so a check run before the fill saw `0 vCPUs, 32 MiB`, read it as a partial,
// and returned without checking anything. `--mem 32` was accepted and the
// machine was rebuilt at a size no guest can boot.
func validateSize(vcpus, memMiB int) error {
	if err := validateBounds(vcpus, memMiB); err != nil {
		return err
	}
	// A machine with memory and no vCPU, or the reverse, cannot boot. Zero on
	// BOTH is how a caller says "change nothing", and the caller resolves both
	// before this runs.
	if (vcpus == 0) != (memMiB == 0) {
		return fmt.Errorf("%w: a machine needs both vCPUs and memory; got %d and %d MiB",
			ErrInvalid, vcpus, memMiB)
	}
	if vcpus > 0 && memMiB < MinMemMiB {
		return fmt.Errorf("%w: %d MiB is too little for a guest to boot; the smallest is %d",
			ErrInvalid, memMiB, MinMemMiB)
	}
	return nil
}

// orgOf is the machine's owning org, for the meter. Empty for a machine that
// predates tenancy, which meters under the empty org rather than not at all.
func (m *Manager) orgOf(ctx context.Context, id string) string {
	if t, err := m.opts.Store.GetTenancy(ctx, id); err == nil && t != nil {
		return t.OrgID
	}
	return ""
}

// captureDiskForResize stores a running machine's disk as a build and returns
// its id, or "" when it wrote nothing since its last one.
//
// The guest is synced first, so writes still in its own page cache reach the
// disk, then paused, so the capture describes one instant. It stays paused:
// the caller kills it next. A failure resumes it, because the caller then
// leaves it running at its old size rather than lose its disk.
func (m *Manager) captureDiskForResize(ctx context.Context, row *state.Machine,
	fcm *fc.Machine) (string, error) {

	if fcm.Slot != nil {
		m.execSync(ctx, row.ID, fcm.Slot)
	}
	t, err := m.templateFor(ctx, row)
	if err != nil {
		return "", err
	}
	if err := fcm.Client.Pause(ctx); err != nil {
		return "", fmt.Errorf("pause: %w", err)
	}
	resume := func() {
		if rerr := fcm.Client.Resume(context.WithoutCancel(ctx)); rerr != nil {
			slog.Error("could not resume a machine whose resize was abandoned",
				"machine", row.ID, "err", rerr)
		}
	}
	rootfs, err := fcm.ChunkifyDisk(ctx, fc.SnapshotOpts{
		RootfsTemplateDir: m.rootfsTemplateDir(t), BuildDir: m.buildDir(),
	})
	if err != nil {
		resume()
		return "", err
	}
	if rootfs == uuid.Nil {
		return "", nil
	}
	if err := m.uploadBuild(ctx, rootfs); err != nil {
		resume()
		return "", err
	}
	return rootfs.String(), nil
}
