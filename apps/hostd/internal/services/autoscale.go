package services

import (
	"context"
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
type Load interface {
	InFlight(machineID string) int
}

// RunAutoscaler keeps each service's running replica count between its floor
// and its demand, until ctx is done.
//
// Only the service's arbiter acts on it. Every host can see every service in
// its local replica, so without that rule an N-host fleet would make N
// simultaneous decisions about one service and start N replicas for one
// overflow. Coordinators propose, hosts dispose: the arbiter decides, and the
// host that owns a replica is the one that starts or stops it.
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
		if owner, ok := state.OwnerFor(svc.ID, live); !ok || owner != m.opts.HostID {
			continue
		}
		if err := m.scaleService(ctx, load, &svc, idleSince); err != nil {
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
func Decide(knobs api.Knobs, replicas []Replica, now time.Time,
	idleSince map[string]time.Time) Decision {
	var running, overLimit int
	var idlest string
	var idlestSince time.Time

	for _, r := range replicas {
		if !r.Running {
			continue
		}
		running++
		if knobs.SoftLimit > 0 && r.InFlight >= knobs.SoftLimit {
			overLimit++
		}
		if r.InFlight == 0 {
			since, seen := idleSince[r.ID]
			if !seen {
				idleSince[r.ID] = now
				since = now
			}
			if idlest == "" || since.Before(idlestSince) {
				idlest, idlestSince = r.ID, since
			}
		} else {
			delete(idleSince, r.ID)
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
	// Give one back only when it has been idle for a while AND doing so stays
	// at or above the floor.
	if idlest != "" && running > knobs.MinMachinesRunning &&
		now.Sub(idlestSince) >= idleBeforeScaleDown {
		return Decision{Down: idlest}
	}
	return Decision{}
}

// Replica is one machine of a service, as the controller sees it.
type Replica struct {
	ID       string
	Running  bool
	InFlight int
}

func (m *Manager) scaleService(ctx context.Context, load Load, svc *state.Service,
	idleSince map[string]time.Time) error {

	machines, err := m.replicasOf(ctx, svc.ID, svc.ReleaseID)
	if err != nil {
		return err
	}
	knobs := api.DefaultKnobs()
	replicas := make([]Replica, 0, len(machines))
	for i, mach := range machines {
		if i == 0 && mach.KindKnobs != "" {
			knobs = api.ParseKnobs(mach.KindKnobs)
		}
		replicas = append(replicas, Replica{
			ID:       mach.ID,
			Running:  mach.State == "running",
			InFlight: load.InFlight(mach.ID),
		})
	}

	switch d := Decide(knobs, replicas, time.Now(), idleSince); {
	case d.Up:
		return m.scaleUp(ctx, svc, machines)
	case d.Down != "":
		delete(idleSince, d.Down)
		// Suspended, never destroyed: destroying the last machine that
		// references a service takes the row -- and its sealed environment --
		// with it, and a scale-down is not a delete.
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
		if mach.State == "suspended" || mach.State == "stopped" {
			return m.opts.Machines.Wake(ctx, mach.ID)
		}
	}
	rel, err := m.opts.Store.GetRelease(ctx, svc.ReleaseID)
	if err != nil {
		return err
	}
	_, err = m.createReplica(ctx, svc, rel, rel.MemBuildID != "")
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
