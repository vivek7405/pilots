package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// ScaleInterval is how often the controller reconsiders a service.
//
// Slower than the router's wake path on purpose. Overflow is handled
// synchronously by wake-on-request -- a request never waits for this loop --
// so the loop's job is only to keep capacity ahead of demand and to give it
// back when demand goes away. A tighter interval would trade real money for
// reacting to noise.
const ScaleInterval = 10 * time.Second

// idleBeforeScaleDown is how long a replica must carry no traffic before it is
// given back.
//
// Sustained, not instantaneous: scaling down on a single quiet tick means a
// service with bursty traffic spends its life stopping and starting replicas,
// which costs more than the replica did.
const idleBeforeScaleDown = 3 * ScaleInterval

// Load is where the controller learns how busy a replica is.
//
// Both numbers are only right on the host that holds the replica: a request
// is counted where it is served, and a flow is visible only in the root
// namespace it crosses. That is why scale-down is decided by the owner.
type Load interface {
	// InFlight is requests the router and exec are serving. It drives the
	// concurrency ceiling and the idle check.
	InFlight(machineID string) int
	// Held is open guest-to-guest TCP sessions to the machine. It keeps a
	// replica from being given back and never scales anything up: twenty
	// pooled connections to a database are not twenty requests.
	Held(machineID string) int
}

// RunAutoscaler keeps each service's running replica count between its floor
// and its demand, until ctx is done.
//
// Every host can see every service in its local replica, so without a rule
// about who acts an N-host fleet would make N simultaneous decisions about one
// service. Scale-up is the arbiter's alone, so N hosts cannot start N replicas
// for one overflow. Scale-down is the owner's alone, because only the owner
// can see whether its replica is busy. Every host runs the loop and each acts
// on the half that is its own.
func (m *Manager) RunAutoscaler(ctx context.Context, load Load) {
	tick := time.NewTicker(ScaleInterval)
	defer tick.Stop()

	idleSince := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := m.scaleOnce(ctx, load, idleSince); err != nil {
			slog.Debug("autoscale pass failed", "err", err)
		}
	}
}

func (m *Manager) scaleOnce(ctx context.Context, load Load, idleSince map[string]time.Time) error {
	svcs, err := m.opts.Store.ListServices(ctx)
	if err != nil {
		return err
	}
	hosts, err := m.opts.Store.ListHosts(ctx)
	if err != nil {
		return err
	}
	live := liveOnly(hosts)

	for i := range svcs {
		svc := svcs[i]
		owner, ok := state.OwnerFor(svc.ID, live)
		arbiter := ok && owner == m.opts.HostID
		if err := m.scaleService(ctx, load, &svc, arbiter, idleSince); err != nil {
			slog.Warn("could not scale a service", "service", svc.ID, "err", err)
		}
	}
	return nil
}

// Decision is what the controller concluded, separated from acting on it so
// the policy can be tested without a machine layer.
type Decision struct {
	Up   bool
	Down string // machine to give back, empty for none
}

// Decide is the whole policy, as a pure function.
//
// minMachinesRunning is a FLOOR, not a target: scale-down clamps at it and
// scale-up never consults it. Getting that backwards means a service with a
// floor of 1 and demand for 5 keeps being cut back to 1.
// Knobs come from the replicas rather than the service row, because that is
// where they live: services has no knobs column, and adding one to a populated
// Corrosion table is the cr-sqlite backfill that must never happen. Every
// replica of a service is created with the same knobs by the rollout, so any
// of them answers for the service.
// Every host runs this over the same replicated rows. The idlest running
// replica fleet-wide, by last_activity then id, is the only one that may be
// given back, and only by the host that holds it, so exactly one host
// concludes it is the one to act.
func Decide(knobs api.Knobs, replicas []Replica, now time.Time,
	idleSince map[string]time.Time) Decision {
	var running, overLimit int
	var idlest string
	var idlestSince time.Time
	var idlestRemote bool

	for _, r := range replicas {
		if !r.Running {
			continue
		}
		running++
		if knobs.SoftLimit > 0 && r.InFlight >= knobs.SoftLimit {
			overLimit++
		}
		if r.InFlight > 0 || r.Held > 0 {
			delete(idleSince, r.ID)
			continue
		}
		// The RANKING key is last_activity alone, because it is the only
		// clock every host reads the same way. Layering the local
		// observation clock on top of it here made the ranking disagree
		// between hosts: idleSince is never earlier than last_activity, so a
		// host ranked its OWN replica less idle than an equally idle remote
		// one. Two hosts holding one replica each therefore both saw the
		// other's as idlest, both declined on !idlestRemote, and the service
		// never scaled down at all.
		//
		// The local clock is still read -- as a dwell guard below, on the
		// replica this host would act on -- so one quiet observation is
		// still not idleness.
		if !r.Remote {
			if _, ok := idleSince[r.ID]; !ok {
				idleSince[r.ID] = now
			}
		}
		since := r.LastActivity
		if idlest == "" || since.Before(idlestSince) ||
			(since.Equal(idlestSince) && r.ID < idlest) {
			idlest, idlestSince, idlestRemote = r.ID, since, r.Remote
		}
	}

	// Below the floor is not a scaling decision, it is a repair: bring one
	// back regardless of load.
	if running < knobs.MinMachinesRunning {
		return Decision{Up: true}
	}
	// Every running replica at its limit means the next request queues behind
	// one that is already full.
	if running > 0 && overLimit == running && knobs.SoftLimit > 0 {
		return Decision{Up: true}
	}
	// Give one back only when it has been idle for a while by the replicated
	// clock AND by this host's own observation of it, doing so stays at or
	// above the floor, the policy allows stopping at all, and the idlest
	// replica fleet-wide is one THIS host holds.
	if idlest != "" && !idlestRemote && knobs.AutoStop != "off" &&
		running > knobs.MinMachinesRunning &&
		now.Sub(idlestSince) >= idleBeforeScaleDown &&
		now.Sub(idleSince[idlest]) >= idleBeforeScaleDown {
		return Decision{Down: idlest}
	}
	return Decision{}
}

// Replica is one machine of a service, as the controller sees it.
type Replica struct {
	ID       string
	Running  bool
	InFlight int
	// Held is open guest-to-guest sessions; see Load.
	Held int
	// LastActivity is the replicated last_activity column, the one idle
	// signal every host can read. Zero means unknown and counts as old.
	LastActivity time.Time
	// Remote is a replica another host holds. Its InFlight and Held are
	// unknown here and read as zero; only its LastActivity is trusted.
	Remote bool
}

func (m *Manager) scaleService(ctx context.Context, load Load, svc *state.Service,
	arbiter bool, idleSince map[string]time.Time) error {

	// A rollout owns this service's machines until it returns. Adding capacity
	// under it would race the gate, and for a volume-backed service would be
	// refused at the claim anyway.
	if m.isRolling(svc.ID) {
		return nil
	}

	machines, err := m.replicasOf(ctx, svc.ID, svc.ReleaseID)
	if err != nil {
		return err
	}
	knobs := api.DefaultKnobs()
	replicas := make([]Replica, 0, len(machines))
	var mine bool
	for i, mach := range machines {
		if i == 0 && mach.KindKnobs != "" {
			knobs = api.ParseKnobs(mach.KindKnobs)
		}
		r := Replica{
			ID: mach.ID, Running: mach.State == "running",
			LastActivity: time.Unix(mach.LastActivity, 0),
			Remote:       mach.HostID != m.opts.HostID,
		}
		if !r.Remote {
			mine = true
			r.InFlight, r.Held = load.InFlight(mach.ID), load.Held(mach.ID)
			// A busy replica keeps its row's last_activity fresh, so every
			// other host ranks it as busy too. A narrow write to this host's
			// own row, once per tick, only while it is busy.
			if r.Running && (r.InFlight > 0 || r.Held > 0) {
				m.opts.Machines.Touch(ctx, mach.ID)
			}
		}
		replicas = append(replicas, r)
	}
	if !arbiter && !mine {
		return nil
	}

	switch d := Decide(knobs, replicas, time.Now(), idleSince); {
	case d.Up:
		// Only the arbiter adds capacity, or N hosts would add N replicas.
		if !arbiter {
			return nil
		}
		return m.scaleUp(ctx, svc, machines)
	case d.Down != "":
		delete(idleSince, d.Down)
		// Suspended, never destroyed: destroying the last machine that
		// references a service takes the row -- and its sealed environment --
		// with it, and a scale-down is not a delete.
		//
		// Always local. Decide names only a replica this host holds, because
		// the owner is the one host that can see whether it is busy and the
		// one host entitled to write its row.
		return m.opts.Machines.Suspend(ctx, d.Down)
	}
	return nil
}

// scaleUp wakes a suspended replica if there is one, and only creates a new
// machine when there is not.
//
// Waking is strictly cheaper: the machine already exists, its snapshot is
// local, and it keeps the identity it had. Creating is the fallback for a
// service that has never had that many replicas.
func (m *Manager) scaleUp(ctx context.Context, svc *state.Service, machines []state.Machine) error {
	for _, mach := range machines {
		if mach.State != "suspended" && mach.State != "stopped" {
			continue
		}
		// Only its OWNER may wake it. The arbiter for a service is
		// hash(id) mod live_hosts and moves as hosts come and go, so the host
		// deciding to scale is routinely not the host holding the replicas --
		// and Wake restores the guest locally before any row write is refused,
		// which means a second copy of a machine already running elsewhere.
		// The waker enforces the same check for the same reason.
		if mach.HostID != m.opts.HostID {
			if err := m.remote(ctx, mach.HostID, mach.ID, "wake"); err != nil {
				return fmt.Errorf("services: waking %s on %s: %w", mach.ID, mach.HostID, err)
			}
			return nil
		}
		return m.opts.Machines.Wake(ctx, mach.ID)
	}
	// A volume-backed service has one machine. If it exists and is not
	// suspended it is running, or it belongs to a rollout or a rescue; a
	// second one could not mount the volume anyway, and the create would be
	// refused at the claim once a tick, forever.
	vol := m.volumeOf(ctx, svc.ID)
	if vol != "" {
		mach, err := m.machineOf(ctx, svc.ID)
		if err != nil || mach != nil {
			return err
		}
	}

	rel, err := m.opts.Store.GetRelease(ctx, svc.ReleaseID)
	if err != nil {
		return err
	}
	_, err = m.createReplica(ctx, svc, rel, rel.MemBuildID != "" && vol == "",
		m.replicaKnobs(ctx, svc, nil), vol)
	return err
}

func liveOnly(hosts []state.Host) []state.Host {
	out := make([]state.Host, 0, len(hosts))
	for _, h := range hosts {
		if time.Since(time.Unix(h.LastSeen, 0)) < 90*time.Second {
			out = append(out, h)
		}
	}
	return out
}
