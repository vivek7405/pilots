package services

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/api"
	"github.com/pilotsrun/pilots/hostd/internal/selfheal"
	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// What an ordinal's volume is, when the rollout has to make one.
//
// A size and a path rather than a knob, because the caller that could set them
// is the deploy, and a deploy that could give ordinal 2 a different size from
// ordinal 1 would be a cluster whose members disagree about how much room they
// have. The recipe sets the size it wants on the service; this is the floor for
// an ordinal the recipe never described.
const (
	ordinalVolumeGiB = 10
	ordinalMountPath = "/var/lib/pilots/data"
)

// Rolling out a service that has one volume per ordinal.
//
// # Why ordinal 1 goes first, and alone
//
// A follower joins by copying from a leader. If ordinals 2..N were created
// alongside 1, they would start looking for a primary that does not exist yet,
// spend their startup failing, and back off — and the cluster would come up
// eventually, by accident, in a time nobody can predict. Ordinal 1 is rolled
// out and gated first, so every follower after it finds something to copy from.
//
// # Why a volume is created before its binding is written
//
// The binding row is write-once, and it names a volume. Writing it first would
// name a volume that does not exist, and nothing would ever be able to correct
// it. The volume exists, then the row names it.
//
// # Why ordinals spread across hosts
//
// Replicas of one service on one host are replicas of one service that die
// together. The placement is derived rather than chosen, from the service id
// and the ordinal, so every host computes the same answer without asking
// anybody — and it wraps only when the fleet is smaller than the replica count,
// which is a real fleet being too small rather than a scheduling decision.

// ordinalHost is where ordinal i of a service belongs.
//
// Deterministic on purpose. Two hosts computing this must agree without
// talking, which is the same property machine ownership already relies on, and
// for the same reason: an arrangement that needs a conversation needs a
// coordinator.
func ordinalHost(serviceID string, ordinal int, live []state.Host) string {
	ids := selfheal.SortedLiveIDs(live)
	if len(ids) == 0 {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(serviceID))
	base := int(h.Sum32() % uint32(len(ids)))
	return ids[(base+ordinal-1)%len(ids)]
}

// OrdinalHostsFor is the placement for a whole service, exported so a test can
// assert the property this exists for: distinct hosts while the fleet has
// enough of them.
func OrdinalHostsFor(serviceID string, replicas int, live []state.Host) []string {
	out := make([]string, 0, replicas)
	for i := 1; i <= replicas; i++ {
		out = append(out, ordinalHost(serviceID, i, live))
	}
	return out
}

// rollOutOnVolumes deploys a service whose engine replicates between ordinals.
//
// The single-volume path is unchanged and still handles every other
// volume-backed service; this one is taken only when the engine label says the
// engine replicates itself, which the API has already checked.
func (m *Manager) rollOutOnVolumes(ctx context.Context, svc *state.Service, rel *state.Release,
	health HealthSpec, knobs []byte, replicas int) error {

	merged, err := m.replicaKnobs(ctx, svc, knobs)
	if err != nil {
		return err
	}

	// Ordinal 1 first, and gated, so followers find a leader to copy from.
	if err := m.rollOutOrdinal(ctx, svc, rel, health, merged, 1); err != nil {
		return fmt.Errorf("services: ordinal 1 of %s: %w", svc.ID, err)
	}

	for i := 2; i <= replicas; i++ {
		if err := m.rollOutOrdinal(ctx, svc, rel, health, merged, i); err != nil {
			return fmt.Errorf("services: ordinal %d of %s: %w", i, svc.ID, err)
		}
	}

	// Ordinals ABOVE the count, on a scale down. Done last, so a shrink that
	// fails mid-way has already brought the survivors onto the new release
	// rather than leaving a cluster split across two.
	if err := m.pruneOrdinals(ctx, svc, replicas); err != nil {
		// Reported, not fatal: the cluster the operator asked for is running,
		// and a leftover machine is a bill rather than an outage.
		slog.Warn("could not prune ordinals above the replica count",
			"service", svc.ID, "replicas", replicas, "err", err)
	}

	rel.Healthy = true
	return m.opts.Store.PutRelease(ctx, rel)
}

// rollOutOrdinal brings one ordinal onto a release.
func (m *Manager) rollOutOrdinal(ctx context.Context, svc *state.Service, rel *state.Release,
	health HealthSpec, knobs []byte, ordinal int) error {

	// Where this ordinal belongs, computed the same way on every host.
	//
	// Nothing read this before. ordinalHost and OrdinalHostsFor existed, the
	// file comment above explained at length why ordinals spread across hosts,
	// and the only caller of either was their own test: every ordinal's volume
	// and every ordinal's machine were created LOCALLY, on whichever host the
	// rollout ran on. So `pilot db ha` built a three-node cluster on one box
	// and printed a confirmation saying the nodes would be "placed on a
	// different host where the fleet allows" -- which is the whole feature,
	// and the one property a high-availability cluster is bought for.
	//
	// Empty when there is no live host list to hash over, which keeps the
	// single-box case exactly as it was.
	host := m.ordinalPlacement(ctx, svc.ID, ordinal)

	volumeID, err := m.ensureOrdinalVolume(ctx, svc, ordinal, host)
	if err != nil {
		return err
	}

	mach, err := m.machineForOrdinal(ctx, svc.ID, volumeID)
	if err != nil {
		return err
	}
	if mach == nil {
		// On the volume's host, not on this one. A volume has exactly one
		// writer, so the replica that mounts it has no choice of host.
		created, err := m.createReplicaOn(ctx, svc, rel, knobs, volumeID, host)
		if err != nil {
			return err
		}
		return withRelease(m.waitHealthy(ctx, created.ID, health), svc.ID, rel.ID)
	}

	// The write-ahead log is flushed before the process is killed.
	//
	// Sixty seconds of exposure is the right trade in ordinary running, and the
	// WRONG one at a moment we control: a redeploy kills this node on purpose,
	// and losing up to a minute of writes to a PLANNED operation is a loss
	// nobody agreed to. `x-pilots.pre_deploy` cannot do this, because it runs
	// on a throwaway machine rather than on the one about to be killed.
	m.flushBeforeKill(ctx, mach)

	// An existing ordinal is REDEPLOYED in place, keeping its volume and its
	// name. The name matters beyond tidiness: the engine identifies a member by
	// it, so an ordinal that came back under a new name would join as a new
	// member and leave the old one in the cluster for ever.
	if err := m.redeploy(ctx, mach, rel); err != nil {
		return err
	}
	return withRelease(m.waitHealthy(ctx, mach.ID, health), svc.ID, rel.ID)
}

// ordinalPlacement is the host this ordinal belongs on, or "" when there is
// nothing to place over.
//
// A failure to read the host list is NOT a placement: it returns "", which
// keeps the ordinal here. Guessing a host from a partial view would put two
// members of one cluster on one box while telling the operator they were
// spread, which is worse than a cluster that is honestly local.
func (m *Manager) ordinalPlacement(ctx context.Context, serviceID string, ordinal int) string {
	hosts, err := m.opts.Store.ListHosts(ctx)
	if err != nil {
		slog.Warn("could not read the host list to place an ordinal; leaving it here",
			"service", serviceID, "ordinal", ordinal, "err", err)
		return ""
	}
	return ordinalHost(serviceID, ordinal, state.LiveHosts(hosts))
}

// ensureOrdinalVolume finds or creates the volume for one ordinal, and returns
// its id.
//
// host is where the volume belongs. A volume is written by exactly one machine
// on exactly one host, so creating it here and then placing the replica
// elsewhere would be a cluster whose members cannot reach their own data.
func (m *Manager) ensureOrdinalVolume(ctx context.Context, svc *state.Service, ordinal int, host string) (string, error) {
	bindings, err := m.opts.Store.ListServiceVolumes(ctx)
	if err != nil {
		return "", err
	}
	for _, b := range bindings {
		if b.ServiceID == svc.ID && b.Ordinal == ordinal {
			return b.VolumeID, nil
		}
	}
	// The volume FIRST, then the row that names it: the binding is write-once,
	// so a row naming a volume that was never created could never be corrected.
	//
	// Named after the service and the ordinal, so somebody looking at a volume
	// list can tell which member of which cluster it belongs to without
	// following two rows to find out.
	req := api.CreateVolumeRequest{
		Name:      fmt.Sprintf("%s-%d", svc.Name, ordinal),
		SizeGiB:   ordinalVolumeGiB,
		MountPath: ordinalMountPath,
	}
	v, err := m.createVolumeOn(ctx, req, host)
	if err != nil {
		return "", fmt.Errorf("services: volume for ordinal %d: %w", ordinal, err)
	}
	// Owned by the service's org, written here rather than by the create. The
	// create records an org only when the request names one, and the org is
	// never sent over the wire to a peer, so an ordinal volume was owned by
	// nobody: its org could not see it, and the quota never counted it. The
	// row is write-once and any host may write it.
	if org, err := m.orgOfService(ctx, svc.ID); err != nil {
		return "", err
	} else if org != "" {
		if err := m.opts.Store.PutTenancy(ctx, &state.Tenancy{
			ID: v.ID, OrgID: org, Kind: "volume", CreatedAt: time.Now().Unix(),
		}); err != nil {
			return "", fmt.Errorf("services: owning ordinal %d's volume %s: %w", ordinal, v.ID, err)
		}
	}
	if err := m.opts.Store.PutServiceVolume(ctx, &state.ServiceVolume{
		ServiceID: svc.ID, Ordinal: ordinal, VolumeID: v.ID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		return "", fmt.Errorf("services: binding ordinal %d to %s: %w", ordinal, v.ID, err)
	}
	return v.ID, nil
}

// createVolumeOn makes a volume here, or on the host a placement named.
func (m *Manager) createVolumeOn(ctx context.Context, req api.CreateVolumeRequest,
	hostID string) (*state.Volume, error) {

	if hostID == "" || hostID == m.opts.HostID {
		return m.opts.Machines.CreateVolume(ctx, req)
	}
	if m.opts.Peers == nil {
		return nil, fmt.Errorf("services: the volume belongs on %s and this host "+
			"cannot reach it", hostID)
	}
	var out state.Volume
	if err := m.opts.Peers.PostJSONReply(ctx, hostID, "/v1/volumes", req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, fmt.Errorf("services: %s created a volume but named none", hostID)
	}
	return &out, nil
}

// machineForOrdinal is the machine mounting one ordinal's volume, or nil.
//
// Found by the VOLUME rather than by a name or an index, because the volume is
// what makes an ordinal that ordinal: a machine that was recreated still holds
// the same data if it mounts the same volume, whatever it is called.
func (m *Manager) machineForOrdinal(ctx context.Context, serviceID, volumeID string) (*state.Machine, error) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		row := &rows[i]
		if row.ServiceID == serviceID && row.VolumeID == volumeID &&
			row.State != state.StateDestroyed {
			return row, nil
		}
	}
	return nil, nil
}

// pruneOrdinals destroys the machines and volumes above a replica count.
//
// Both, and that is a decision rather than an oversight. An ordinal's volume
// holds a COPY of data the surviving ordinals also hold, so keeping it would be
// paying to store a third copy of something nobody will read. Scaling back up
// creates a fresh volume and the engine fills it from the leader, which is what
// it would do with the old one anyway after any real interval.
func (m *Manager) pruneOrdinals(ctx context.Context, svc *state.Service, replicas int) error {
	bindings, err := m.opts.Store.ListServiceVolumes(ctx)
	if err != nil {
		return err
	}
	var extra []state.ServiceVolume
	for _, b := range bindings {
		if b.ServiceID == svc.ID && b.Ordinal > replicas {
			extra = append(extra, b)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	// Highest ordinal first, so a prune interrupted half way has removed the
	// end of the range rather than a hole in the middle of it.
	sort.Slice(extra, func(i, j int) bool { return extra[i].Ordinal > extra[j].Ordinal })

	var errs []error
	for _, b := range extra {
		mach, err := m.machineForOrdinal(ctx, svc.ID, b.VolumeID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if mach != nil {
			if err := m.opts.Machines.Destroy(ctx, mach.ID); err != nil {
				errs = append(errs, fmt.Errorf("destroying ordinal %d (%s): %w", b.Ordinal, mach.ID, err))
				continue
			}
		}
		// The volume itself, as this function's own doc has always said. Only
		// the machine and the binding went, and the destroy only detaches a
		// volume, so every scale-down left a full database copy and its
		// snapshots in object storage with nothing able to reach it. Before
		// the binding: a delete that fails keeps the binding, and the next
		// prune finds the ordinal above the count and tries again.
		if err := m.opts.Machines.DeleteVolume(ctx, b.VolumeID); err != nil && !errors.Is(err, state.ErrNotFound) {
			errs = append(errs, fmt.Errorf("deleting ordinal %d's volume %s: %w", b.Ordinal, b.VolumeID, err))
			continue
		}
		if err := m.opts.Store.DeleteServiceVolume(ctx, svc.ID, b.Ordinal); err != nil {
			errs = append(errs, fmt.Errorf("unbinding ordinal %d: %w", b.Ordinal, err))
		}
		slog.Info("pruned an ordinal above the replica count",
			"service", svc.ID, "ordinal", b.Ordinal, "volume", b.VolumeID)
	}
	return errors.Join(errs...)
}

// replicatesItsOwnOrdinals reports whether an engine replicates between its own
// volumes rather than sharing one.
//
// The same two the API admits, and it is a SHORT list on purpose: every entry
// is an engine somebody has run this way, not an engine somebody believes would
// work. Adding one is a decision, which is what a list rather than a flag makes
// it.
func replicatesItsOwnOrdinals(engine string) bool {
	return engine == "postgres" || engine == "etcd"
}

// engineOf is the write-once pilot.engine label on a service, or empty.
//
// Read rather than passed, because the deploy path is reached from several
// callers and threading a label through all of them would be a parameter that
// most of them pass empty and one of them has to get right.
func (m *Manager) engineOf(ctx context.Context, serviceID string) string {
	labels, err := m.opts.Store.GetLabels(ctx, serviceID)
	if err != nil || labels == nil {
		return ""
	}
	return labels.Labels["pilot.engine"]
}

// flushBeforeKill asks a node to archive what it has not archived yet.
//
// Best effort, and that is a decision rather than laziness. The flush REDUCES a
// loss that is already bounded and already documented; refusing to deploy
// because a node would not flush would turn a smaller exposure into an outage,
// and an operator who asked for a deploy would get neither the deploy nor an
// explanation they could act on.
//
// So a failure is a log line naming the node, and the deploy continues with the
// sixty-second window the recipe already promises.
func (m *Manager) flushBeforeKill(ctx context.Context, mach *state.Machine) {
	if m.opts.Machines == nil {
		return
	}
	res, err := m.opts.Machines.Exec(ctx, mach.ID, api.ExecRequest{
		Cmd: "/usr/local/bin/pilot-pg-flush", User: "root",
	})
	switch {
	case err != nil:
		slog.Warn("could not flush a node's write-ahead log before redeploying it; "+
			"the ordinary sixty-second window applies",
			"machine", mach.ID, "err", err)
	case res.ExitCode != 0:
		slog.Warn("a node did not archive its write-ahead log before being redeployed; "+
			"the ordinary sixty-second window applies",
			"machine", mach.ID, "exit", res.ExitCode, "stderr", res.Stderr)
	default:
		slog.Info("flushed a node's write-ahead log before redeploying it", "machine", mach.ID)
	}
}

// orgOfService is the org that owns a service, or "" for one that has none.
func (m *Manager) orgOfService(ctx context.Context, serviceID string) (string, error) {
	t, err := m.opts.Store.GetTenancy(ctx, serviceID)
	if errors.Is(err, state.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("services: resolve the org of service %s: %w", serviceID, err)
	}
	return t.OrgID, nil
}
