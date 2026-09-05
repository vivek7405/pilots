package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	// Redeploy boots a machine again from another image in place: same row,
	// same URL, same volume. How a volume-backed service takes a release,
	// because a second machine cannot mount the volume beside the first.
	Redeploy(ctx context.Context, id string, req api.RedeployRequest) (*state.Machine, error)
	// Touch records that a machine is in use, as the router does on every
	// request. The autoscaler calls it for a busy replica so the row says so
	// fleet-wide.
	Touch(ctx context.Context, id string)
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
	// Peers reaches another host's internal API. Nil on a single box, where
	// every machine is local and there is nowhere to forward to.
	Peers PeerCaller
}

// PeerCaller performs a lifecycle call against a machine held by another host.
//
// The local Suspend and Wake act on a guest THIS host is running, so calling
// them for a machine held elsewhere either does nothing or starts a second
// copy of it. The arbiter deciding a service's scale is routinely not the host
// holding its replicas, so the distinction is the normal case rather than an
// edge one.
type PeerCaller interface {
	Post(ctx context.Context, hostID, path string) error
	// PostJSON is Post with a body, for a call that names an image.
	PostJSON(ctx context.Context, hostID, path string, body any) error
}

type Manager struct {
	opts Options
	// rolling is every service with a deploy or rollback in flight on this
	// host. The autoscaler steps aside for one, and a second rollout of the
	// same service is refused. In process, because only the arbiter deploys a
	// service and only the arbiter adds capacity to it -- a replicated row
	// for a transient fact would be a second writer of service state and a
	// tombstone to collect.
	mu      sync.Mutex
	rolling map[string]struct{}
}

func New(opts Options) *Manager {
	return &Manager{opts: opts, rolling: map[string]struct{}{}}
}

// beginRollout claims a service for one rollout at a time.
func (m *Manager) beginRollout(serviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rolling[serviceID]; ok {
		return fmt.Errorf("services: a rollout of %s is already in progress", serviceID)
	}
	m.rolling[serviceID] = struct{}{}
	return nil
}

func (m *Manager) endRollout(serviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rolling, serviceID)
}

// isRolling reports whether a rollout of this service owns its machines.
func (m *Manager) isRolling(serviceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rolling[serviceID]
	return ok
}

// Deploy rolls a service onto a release, and flips its route only once the new
// replicas have proved they serve.
//
// The order is the whole design. Flipping first and checking after is a
// partial outage that reads as a successful deploy in the logs, so: boot one,
// gate it, snapshot it, restore the rest from that snapshot, gate those,
// THEN flip. The previous release's machines are stopped but kept, so a
// rollback is a start-and-flip rather than a rebuild.
func (m *Manager) Deploy(ctx context.Context, serviceID, rootfsBuildID string,
	knobs json.RawMessage) (*state.Release, error) {
	svc, err := m.opts.Store.GetService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if err := m.beginRollout(serviceID); err != nil {
		return nil, err
	}
	defer m.endRollout(serviceID)

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

	// A service that mounts a volume has one machine and it is replaced in
	// place: a second machine could not mount the volume beside it, and
	// destroying the first would take the service row with it. So there is no
	// previous set to suspend and nothing to prune.
	if vol := m.volumeOf(ctx, svc.ID); vol != "" {
		if err := m.rollOutOnVolume(ctx, svc, rel, health, knobs, vol); err != nil {
			return nil, err
		}
		if err := m.opts.Store.CASServiceRelease(ctx, svc.ID, svc.ReleaseID, rel.ID); err != nil {
			return nil, fmt.Errorf("services: another deploy moved %s: %w", svc.ID, err)
		}
		return rel, nil
	}

	// The machines of the release we are replacing, captured BEFORE anything
	// changes: on success they are stopped, on failure the new ones are.
	previous, err := m.replicasOf(ctx, svc.ID, svc.ReleaseID)
	if err != nil {
		return nil, err
	}

	fresh, err := m.rollOut(ctx, svc, rel, health, replicas, knobs)
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
	health HealthSpec, replicas int, knobs json.RawMessage) ([]string, error) {

	var created []string

	// Resolved ONCE, for the whole rollout. Per replica it would drift: during
	// a rollout the service still names the previous release, so replica two
	// would inherit from that one while replica one carried what the deploy
	// asked for.
	knobs = m.replicaKnobs(ctx, svc, knobs)

	// Replica 1 BOOTS: a release has no memory image until something has
	// proved this rootfs serves.
	first, err := m.createReplica(ctx, svc, rel, false, knobs, "")
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
		r, err := m.createReplica(ctx, svc, rel, rel.MemBuildID != "", knobs, "")
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

// volumeOf is the volume a service mounts, or empty.
func (m *Manager) volumeOf(ctx context.Context, serviceID string) string {
	sv, err := m.opts.Store.ServiceVolume(ctx, serviceID)
	if err != nil || sv == nil {
		return ""
	}
	return sv.VolumeID
}

// machineOf is a volume-backed service's ONE machine, in any release and any
// state, or nil when it has none.
//
// Any release, because during a rollout the machine already carries the new
// one; any state, because a suspended or erroring replica is still the machine
// that holds the claim. Two is the invariant this whole path exists to keep,
// broken, so it is refused loudly rather than resolved by picking one.
func (m *Manager) machineOf(ctx context.Context, serviceID string) (*state.Machine, error) {
	all, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	var found *state.Machine
	for i := range all {
		mach := all[i]
		if mach.ServiceID != serviceID || mach.State == state.StateDestroyed {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("services: %s has two machines, %s and %s, and "+
				"it mounts a volume that only one of them can hold",
				serviceID, found.ID, mach.ID)
		}
		found = &all[i]
	}
	return found, nil
}

// rollOutOnVolume puts a release on a volume-backed service's one machine.
//
// Boot, never restore: the release keeps no memory build, because a checkpoint
// of a volume machine carries the drive in its device state and nothing may
// restore it into another machine. The window between the kill and the gate is
// the price of one volume (ARCHITECTURE.md); a request arriving inside it is
// held by the router on the machine's own lock. On a failed gate the machine
// goes back on the release it was on, on the same volume, and both errors are
// reported.
func (m *Manager) rollOutOnVolume(ctx context.Context, svc *state.Service, rel *state.Release,
	health HealthSpec, knobs json.RawMessage, volumeID string) error {

	mach, err := m.machineOf(ctx, svc.ID)
	if err != nil {
		return err
	}
	if mach == nil {
		// A first deploy. On a failed gate the machine is LEFT in place:
		// destroying the only machine of a service deletes the service row,
		// sealed environment included.
		created, err := m.createReplica(ctx, svc, rel, false, m.replicaKnobs(ctx, svc, knobs), volumeID)
		if err != nil {
			return fmt.Errorf("services: first replica of %s: %w", rel.ID, err)
		}
		if err := m.waitHealthy(ctx, created.ID, health); err != nil {
			return err
		}
	} else {
		if err := m.redeploy(ctx, mach, rel); err != nil {
			return fmt.Errorf("services: redeploying %s onto %s: %w", mach.ID, rel.ID, err)
		}
		if err := m.waitHealthy(ctx, mach.ID, health); err != nil {
			prev, perr := m.opts.Store.GetRelease(ctx, svc.ReleaseID)
			if perr != nil {
				return errors.Join(err, perr)
			}
			if rerr := m.redeploy(ctx, mach, prev); rerr != nil {
				return errors.Join(err, rerr)
			}
			return errors.Join(err, m.waitHealthy(ctx, mach.ID, health))
		}
	}
	rel.Healthy = true
	return m.opts.Store.PutRelease(ctx, rel)
}

// redeploy boots one machine again from a release's image, here or on the
// host that holds it.
//
// After a rescue the arbiter and the owner routinely differ, so the remote
// branch is the normal case rather than an edge one. No knobs travel: the row
// keeps the policy it has, exactly as a wake does.
func (m *Manager) redeploy(ctx context.Context, mach *state.Machine, rel *state.Release) error {
	req := api.RedeployRequest{Image: rel.RootfsBuildID, Release: rel.ID}
	if mach.HostID == m.opts.HostID {
		_, err := m.opts.Machines.Redeploy(ctx, mach.ID, req)
		return err
	}
	if m.opts.Peers == nil {
		return fmt.Errorf("services: %s is held by %s and this host cannot reach it",
			mach.ID, mach.HostID)
	}
	return m.opts.Peers.PostJSON(ctx, mach.HostID, "/v1/machines/"+mach.ID+"/redeploy", req)
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
	rel *state.Release, restore bool, knobs json.RawMessage, volumeID string) (*state.Machine, error) {

	// A volume machine BOOTS (machines.startNewMachine): a drive cannot be
	// added to a snapshot being restored, and Create restores whenever it sees
	// a mem build regardless of the volume, so the choice is made here.
	if volumeID != "" {
		restore = false
	}
	req := api.CreateMachineRequest{
		App:    svc.App,
		Image:  rel.RootfsBuildID,
		Volume: volumeID,
		// Replicas carry the service's lifecycle knobs, which is also where
		// they persist: services has no knobs column and must not grow one.
		// Resolved once per rollout by the caller, so replicas of one release
		// cannot disagree.
		Knobs:   knobs,
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
	if err := m.beginRollout(serviceID); err != nil {
		return nil, err
	}
	defer m.endRollout(serviceID)

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

	health, err := ParseHealth(svc.Health)
	if err != nil {
		return nil, err
	}

	// One machine, redeployed onto the target: a volume-backed service keeps
	// no suspended previous release to wake.
	if vol := m.volumeOf(ctx, serviceID); vol != "" {
		if err := m.rollOutOnVolume(ctx, svc, target, health, nil, vol); err != nil {
			return nil, err
		}
		if err := m.opts.Store.CASServiceRelease(ctx, serviceID, svc.ReleaseID, target.ID); err != nil {
			return nil, err
		}
		return target, nil
	}

	machines, err := m.replicasOf(ctx, serviceID, target.ID)
	if err != nil {
		return nil, err
	}

	if len(machines) == 0 {
		// Its machines were pruned. Still recoverable: the release's build
		// pair is the whole machine, so roll forward onto it instead.
		// A rollback re-runs what was there; it asks for no knobs of its own
		// and inherits whatever the release's surviving siblings carry.
		fresh, err := m.rollOut(ctx, svc, target, health, max(svc.Replicas, 1), nil)
		if err != nil {
			// Same cleanup Deploy does in the same situation. rollOut returns
			// what it created precisely so a failure does not leave half a
			// rollout running and billing with nothing pointing at it.
			for _, id := range fresh {
				if derr := m.opts.Machines.Destroy(ctx, id); derr != nil {
					slog.Error("could not clean up a failed rollback's machine",
						"machine", id, "service", serviceID, "err", derr)
				}
			}
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
	superseded, err := m.replicasOf(ctx, serviceID, svc.ReleaseID)
	if err != nil {
		// The flip already happened, so the rollback succeeded; report the
		// leftovers rather than swallowing them. Left unsaid, the release just
		// rolled away from keeps serving and billing with nothing pointing at
		// it.
		return target, fmt.Errorf("rolled back to %s, but could not list the "+
			"superseded replicas to stop them: %w", target.ID, err)
	}
	for _, mach := range superseded {
		if err := m.opts.Machines.Suspend(ctx, mach.ID); err != nil {
			slog.Warn("could not suspend a superseded replica after rollback",
				"machine", mach.ID, "service", serviceID, "err", err)
		}
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// replicaKnobs is the lifecycle configuration a new replica gets.
//
// Precedence: what the deploy asked for is merged over what an existing
// replica carries, and that over the machine defaults. Inheriting from a
// sibling is what keeps every machine of a service agreeing when nothing was
// asked; the defaults are the sandbox's own (suspend when idle, wake on
// demand, a floor of zero), because a replica IS a sandbox with a release
// attached. The release, not a knob, is what hands its idle decision to the
// autoscaler (machines.shouldSuspend), so nothing here needs to differ. A
// warm replica is a deploy with min_machines_running set.
func (m *Manager) replicaKnobs(ctx context.Context, svc *state.Service,
	requested json.RawMessage) json.RawMessage {

	base := api.DefaultKnobs()
	if machines, err := m.replicasOf(ctx, svc.ID, svc.ReleaseID); err == nil {
		for _, mach := range machines {
			if mach.KindKnobs != "" {
				base = api.ParseKnobs(mach.KindKnobs)
				break
			}
		}
	}
	if len(requested) > 0 {
		// Partial, onto the inherited value: {"min_machines_running":1}
		// changes the floor and nothing else. A decode error here is not
		// the rollout's to swallow; the API validated the body already.
		_ = json.Unmarshal(requested, &base)
	}
	raw, _ := api.MarshalKnobs(base)
	return json.RawMessage(raw)
}

// remote asks another host to suspend or wake one of its machines.
func (m *Manager) remote(ctx context.Context, hostID, machineID, action string) error {
	if m.opts.Peers == nil {
		return fmt.Errorf("services: %s is held by %s and this host cannot reach it",
			machineID, hostID)
	}
	return m.opts.Peers.Post(ctx, hostID, "/v1/machines/"+machineID+"/"+action)
}
