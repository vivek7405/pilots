package main

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// guestLoad is what the autoscaler learns from the root namespace: which
// replicas have open guest-to-guest sessions right now.
type guestLoad struct {
	mu   sync.Mutex
	held map[string]int
}

func (g *guestLoad) Held(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held[id]
}

func (g *guestLoad) set(held map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = held
}

// load joins the router's in-flight count with the namespace's open sessions.
// It is the autoscaler's whole view of a replica.
type load struct {
	*machines.Manager
	guest *guestLoad
}

func (l load) Held(id string) int { return l.guest.Held(id) }

type machineToucher interface {
	Touch(ctx context.Context, id string)
}

// runActivity turns root-namespace traffic into the two signals the
// autoscaler reads: a rising packet count on a running replica's address is a
// Touch on its row, and an established session to it is a held count.
//
// Same tick as the waker, for the same reason: the rules and the counts are
// two halves of one mechanism.
func runActivity(ctx context.Context, view fleetView, loc *mesh.Locator,
	toucher machineToucher, guest *guestLoad) {

	tick := time.NewTicker(wakeInterval)
	defer tick.Stop()

	seen := map[string]uint64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		if counts, err := netns.ActivityCounts(); err != nil {
			slog.Debug("could not read activity counters", "err", err)
		} else {
			for _, id := range risen(seen, counts) {
				toucher.Touch(ctx, id)
			}
		}

		flows, err := netns.OpenFlows()
		if err != nil {
			slog.Debug("could not list open flows", "err", err)
			continue
		}
		running := map[netip.Addr]string{}
		for _, m := range view.Machines() {
			if m.State != "running" {
				continue
			}
			if addr, ok := loc.MachineAddress(m); ok {
				running[addr] = m.ID
			}
		}
		guest.set(netns.HeldBy(flows, running))
	}
}

// risen reports the machines whose count rose since the last reading and
// records the new baselines. A machine seen for the first time only sets its
// baseline: the table is rebuilt, and its counters zeroed, whenever the fleet
// changes, and a reset must not read as traffic.
func risen(seen map[string]uint64, counts map[string]uint64) []string {
	var out []string
	for id, n := range counts {
		last, known := seen[id]
		seen[id] = n
		if known && n > last {
			out = append(out, id)
		}
	}
	for id := range seen {
		if _, still := counts[id]; !still {
			delete(seen, id)
		}
	}
	return out
}
