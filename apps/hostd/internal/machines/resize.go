package machines

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
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
	if err := validateSize(vcpus, memMiB); err != nil {
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
	// The memory image, and ONLY the memory image. The disk is what this
	// machine keeps: its copy-on-write file carries every write since the last
	// snapshot, and a resize is not a redeploy, so it stays.
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

	token := m.token(id)
	if token == templateToken {
		token = newID("agt")
		sum := sha256.Sum256([]byte(token))
		row.AgentTokenHash = hex.EncodeToString(sum[:])
	}

	// Booted from the machine's OWN image, which is what makes this a resize
	// rather than a redeploy: the disk it comes up on is the disk it went down
	// with.
	fcm, err := m.bootMachine(ctx, row, token, row.VolumeID, row.ImageRef, "")
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
	if superseded != "" {
		m.discardBuilds(ctx, superseded)
	}
	return row, nil
}

// validateSize refuses a size no host could hold, at the API's edge rather
// than somewhere inside a boot.
func validateSize(vcpus, memMiB int) error {
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
	// A machine with memory and no vCPU, or the reverse, cannot boot. Zero on
	// BOTH is how a caller says "change only the other one", which the caller
	// above resolves before this runs.
	if (vcpus == 0) != (memMiB == 0) {
		return nil
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
