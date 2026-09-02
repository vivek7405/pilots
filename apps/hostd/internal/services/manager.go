package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// MachineManager is the slice of machines.Manager a rollout needs.
//
// An interface rather than the concrete type so a deploy can be tested without
// a Firecracker: the rollout's logic is ordering and gating, and that is worth
// testing on a machine layer that cannot boot.
type MachineManager interface {
	Create(ctx context.Context, req api.CreateMachineRequest) (*state.Machine, error)
	Destroy(ctx context.Context, id string) error
	// Suspend and Wake are the "stopped but present" pair. Suspend snapshots
	// and releases the machine's resources while keeping its row and identity,
	// which is exactly what a superseded release needs: a rollback is a wake
	// and a route flip rather than a rebuild.
	Suspend(ctx context.Context, id string) error
	Wake(ctx context.Context, id string) error
	Exec(ctx context.Context, machineID string, req api.ExecRequest) (*api.ExecResponse, error)
	Checkpoint(ctx context.Context, machineID, comment string) (*state.Checkpoint, error)
	// AppAddr is where this host can reach the machine's application port,
	// empty if it holds no slot for it.
	AppAddr(machineID string) (string, bool)
	// ResetAgentToken puts the guest's credential back to the placeholder the
	// golden template ships, so a machine restored from this one's snapshot
	// can install its own the same way a template restore does.
	ResetAgentToken(ctx context.Context, machineID string) error
}

type Options struct {
	HostID   string
	Store    state.Store
	Machines MachineManager
}

type Manager struct{ opts Options }

func New(opts Options) *Manager { return &Manager{opts: opts} }

// Deploy rolls a service onto a release, and flips its route only once the new
// replicas have proved they serve.
//
// The order is the whole design. Flipping first and checking after is a
// partial outage that reads as a successful deploy in the logs, so: boot one,
// gate it, snapshot it, restore the rest from that snapshot, gate those,
// THEN flip. The previous release's machines are stopped but kept, so a
// rollback is a start-and-flip rather than a rebuild.
func (m *Manager) Deploy(ctx context.Context, serviceID, rootfsBuildID string) (*state.Release, error) {
	svc, err := m.opts.Store.GetService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	health, err := ParseHealth(svc.Health)
	if err != nil {
		return nil, err
	}
	replicas := svc.Replicas
	if replicas < 1 {
		replicas = 1
	}

	rel := &state.Release{
		ID:            "rel_" + uuid.NewString(),
		ServiceID:     svc.ID,
		RootfsBuildID: rootfsBuildID,
		CreatedAt:     time.Now().Unix(),
	}
	if err := m.opts.Store.PutRelease(ctx, rel); err != nil {
		return nil, err
	}

	// The machines of the release we are replacing, captured BEFORE anything
	// changes: on success they are stopped, on failure the new ones are.
	previous, err := m.replicasOf(ctx, svc.ID, svc.ReleaseID)
	if err != nil {
		return nil, err
	}

	fresh, err := m.rollOut(ctx, svc, rel, health, replicas)
	if err != nil {
		// Nothing has been flipped, so the old release is still serving.
		// Clear up what was half-built rather than leaving it to bill.
		for _, id := range fresh {
			if derr := m.opts.Machines.Destroy(ctx, id); derr != nil {
				slog.Error("could not clean up a failed deploy's machine",
					"machine", id, "service", svc.ID, "err", derr)
			}
		}
		return nil, err
	}

	// Only now. Compare-and-swap on the release the caller saw, so two
	// interleaving deploys cannot leave the service naming one release while
	// the other's machines are the ones running.
	if err := m.opts.Store.CASServiceRelease(ctx, svc.ID, svc.ReleaseID, rel.ID); err != nil {
		for _, id := range fresh {
			_ = m.opts.Machines.Destroy(ctx, id)
		}
		return nil, fmt.Errorf("services: another deploy moved %s: %w", svc.ID, err)
	}

	// Stopped, not destroyed. Rollback is a start-and-flip; pruning here would
	// turn a five-second rollback into a rebuild. They are pruned on the NEXT
	// successful deploy, which is also what bounds how many accumulate.
	for _, mach := range previous {
		if err := m.opts.Machines.Suspend(ctx, mach.ID); err != nil {
			slog.Warn("could not suspend a superseded replica",
				"machine", mach.ID, "service", svc.ID, "err", err)
		}
	}
	m.prune(ctx, svc.ID, rel.ID, svc.ReleaseID)
	return rel, nil
}

// rollOut boots the first replica, gates it, snapshots it, and restores the
// rest from that snapshot. It returns every machine it created so a failed
// deploy can clean up after itself.
func (m *Manager) rollOut(ctx context.Context, svc *state.Service, rel *state.Release,
	health HealthSpec, replicas int) ([]string, error) {

	var created []string

	// Replica 1 BOOTS: a release has no memory image until something has
	// proved this rootfs serves.
	first, err := m.createReplica(ctx, svc, rel, false)
	if err != nil {
		return created, fmt.Errorf("services: first replica of %s: %w", rel.ID, err)
	}
	created = append(created, first.ID)

	if err := m.waitHealthy(ctx, first.ID, health); err != nil {
		return created, err
	}

	// Snapshot it, and every later replica of this release restores instead of
	// booting -- the measured sub-second path rather than a cold boot nobody
	// budgeted. Best effort by design: a release that cannot be snapshotted
	// still deploys, it just deploys the slow way.
	if err := m.snapshotRelease(ctx, first.ID, rel); err != nil {
		slog.Warn("release has no memory image; its replicas will boot rather than restore",
			"service", svc.ID, "release", rel.ID, "err", err)
	}

	rel.Healthy = true
	if err := m.opts.Store.PutRelease(ctx, rel); err != nil {
		return created, err
	}

	for i := 1; i < replicas; i++ {
		r, err := m.createReplica(ctx, svc, rel, rel.MemBuildID != "")
		if err != nil {
			return created, fmt.Errorf("services: replica %d of %s: %w", i+1, rel.ID, err)
		}
		created = append(created, r.ID)
		if err := m.waitHealthy(ctx, r.ID, health); err != nil {
			return created, err
		}
	}
	return created, nil
}

// createReplica makes one machine for a release, restoring when the release
// has a memory image and booting when it does not.
//
// Restore-first, boot-second, and a missing memory image is a FALLBACK rather
// than an error: sprites treats memory snapshots as best-effort and discards
// them on upgrade, disk pressure or migration, and a platform that failed a
// deploy because a snapshot was gone would be worse than one that took the
// slow path.
func (m *Manager) createReplica(ctx context.Context, svc *state.Service,
	rel *state.Release, restore bool) (*state.Machine, error) {

	req := api.CreateMachineRequest{
		App:   svc.App,
		Image: rel.RootfsBuildID,
		// Replicas carry the service's lifecycle knobs, which is also where
		// they persist: services has no knobs column and must not grow one.
		Knobs:   m.replicaKnobs(ctx, svc),
		Service: svc.ID,
		Release: rel.ID,
	}
	if restore {
		// The build pair from the release, not the rootfs alone.
		req.Image = ""
		req.MemBuildID = rel.MemBuildID
		req.RootfsBuildID = rel.RootfsBuildID
	}
	return m.opts.Machines.Create(ctx, req)
}

// snapshotRelease freezes a proved replica into the release's build pair.
func (m *Manager) snapshotRelease(ctx context.Context, machineID string, rel *state.Release) error {
	// Put the guest's credential back to the placeholder FIRST. Every replica
	// restored from this image installs its own token by authenticating as the
	// placeholder -- exactly how a golden-template restore does it -- and a
	// snapshot carrying this machine's own token would lock every later
	// replica out of its own agent.
	if err := m.opts.Machines.ResetAgentToken(ctx, machineID); err != nil {
		return fmt.Errorf("reset agent token before snapshot: %w", err)
	}
	ck, err := m.opts.Machines.Checkpoint(ctx, machineID, "release "+rel.ID)
	if err != nil {
		return err
	}
	if ck.MemBuildID == "" {
		return errors.New("checkpoint produced no memory build")
	}
	rel.MemBuildID = ck.MemBuildID
	if ck.RootfsBuildID != "" {
		rel.RootfsBuildID = ck.RootfsBuildID
	}
	return nil
}

// Rollback returns a service to its previous healthy release.
//
// Start-and-flip: the previous release's machines were stopped rather than
// destroyed, so this is seconds rather than a rebuild. A release that never
// reached its health gate is never a target -- rolling back onto something
// that was never proved to serve is not a rollback.
func (m *Manager) Rollback(ctx context.Context, serviceID string) (*state.Release, error) {
	svc, err := m.opts.Store.GetService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	rels, err := m.opts.Store.ReleasesFor(ctx, serviceID)
	if err != nil {
		return nil, err
	}

	var target *state.Release
	for i := range rels {
		if rels[i].ID != svc.ReleaseID && rels[i].Healthy {
			target = &rels[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("services: %s has no earlier healthy release to roll back to", serviceID)
	}

	machines, err := m.replicasOf(ctx, serviceID, target.ID)
	if err != nil {
		return nil, err
	}
	health, err := ParseHealth(svc.Health)
	if err != nil {
		return nil, err
	}

	if len(machines) == 0 {
		// Its machines were pruned. Still recoverable: the release's build
		// pair is the whole machine, so roll forward onto it instead.
		if _, err := m.rollOut(ctx, svc, target, health, max(svc.Replicas, 1)); err != nil {
			return nil, err
		}
	} else {
		for _, mach := range machines {
			if err := m.opts.Machines.Wake(ctx, mach.ID); err != nil {
				return nil, fmt.Errorf("services: waking %s: %w", mach.ID, err)
			}
		}
		for _, mach := range machines {
			if err := m.waitHealthy(ctx, mach.ID, health); err != nil {
				return nil, err
			}
		}
	}

	if err := m.opts.Store.CASServiceRelease(ctx, serviceID, svc.ReleaseID, target.ID); err != nil {
		return nil, err
	}
	for _, mach := range must(m.replicasOf(ctx, serviceID, svc.ReleaseID)) {
		_ = m.opts.Machines.Suspend(ctx, mach.ID)
	}
	return target, nil
}

// prune destroys the machines of every release except the current one and the
// one being kept as a rollback target.
func (m *Manager) prune(ctx context.Context, serviceID, keepA, keepB string) {
	all, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return
	}
	for _, mach := range all {
		if mach.ServiceID != serviceID {
			continue
		}
		rel := mach.ReleaseID
		if rel == keepA || rel == keepB || rel == "" {
			continue
		}
		if err := m.opts.Machines.Destroy(ctx, mach.ID); err != nil {
			slog.Warn("could not prune a superseded replica",
				"machine", mach.ID, "release", rel, "err", err)
		}
	}
}

// replicasOf lists a service's machines for one release.
func (m *Manager) replicasOf(ctx context.Context, serviceID, releaseID string) ([]state.Machine, error) {
	if releaseID == "" {
		return nil, nil
	}
	all, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	var out []state.Machine
	for _, mach := range all {
		if mach.ServiceID == serviceID && mach.ReleaseID == releaseID {
			out = append(out, mach)
		}
	}
	return out, nil
}

func must[T any](v T, err error) T { return v }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// replicaKnobs is the lifecycle configuration a new replica gets.
//
// Inherited from an existing replica so every machine of a service agrees,
// and defaulted for the first one. auto_stop stays OFF by default for a
// service replica: the autoscaler owns that decision and the idle monitor
// stopping a replica behind its back would fight it.
func (m *Manager) replicaKnobs(ctx context.Context, svc *state.Service) json.RawMessage {
	if machines, err := m.replicasOf(ctx, svc.ID, svc.ReleaseID); err == nil {
		for _, mach := range machines {
			if mach.KindKnobs != "" {
				return json.RawMessage(mach.KindKnobs)
			}
		}
	}
	return json.RawMessage(`{"auto_stop":"off","min_machines_running":1}`)
}
