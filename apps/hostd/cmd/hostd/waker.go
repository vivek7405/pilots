package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// wakeInterval is how often a suspended service replica's drop counter is
// read. It matches the tenant filter's own tick: the rules and the counts are
// two halves of one mechanism and there is nothing to gain from reading them
// at different rates.
const wakeInterval = tenantInterval

// runWaker brings a suspended service replica back when traffic arrives for it.
//
// This is what makes min_machines_running: 0 usable for a service that another
// service depends on. An inbound HTTP request to a suspended machine is held
// by the router while it wakes; a peer connecting over .internal had no such
// path -- it got a name that resolved to an address with nothing behind it.
// Now the address survives the suspend, packets to it are counted and dropped
// in the root namespace, and a rising count is the wake.
//
// The client's own TCP retransmission absorbs the delay: the first SYN is
// dropped, the wake starts, and the retransmit at 1s (then 2s, 4s) lands on a
// machine that is back. Nothing has to hold a connection open, and nothing has
// to understand the protocol -- Postgres works exactly as HTTP does.
func runWaker(ctx context.Context, hostID string, view fleetView, waker machineWaker) {
	tick := time.NewTicker(wakeInterval)
	defer tick.Stop()

	// Last count per machine. A machine absent from this map is one we have
	// not seen suspended yet; its first reading establishes the baseline
	// rather than waking it, because the counter survives from before the
	// machine was suspended.
	seen := map[string]uint64{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		counts, err := netns.WakeCounts()
		if err != nil {
			slog.Debug("could not read wake counters", "err", err)
			continue
		}

		for id, n := range counts {
			last, known := seen[id]
			seen[id] = n
			if !known || n <= last {
				continue
			}

			// Re-check ownership before acting. A rescue can move a machine
			// while a peer still holds a DNS answer pointing here, and waking
			// a machine this host no longer owns would start a second copy of
			// it -- the one failure the whole single-writer discipline exists
			// to prevent. fly hit the same class of race between their proxy's
			// wake decisions and gossip, and had to bounce connections that
			// landed on a machine another node had just stopped.
			row, ok := currentRow(view, id)
			if !ok || row.HostID != hostID {
				delete(seen, id)
				continue
			}
			if row.State != "suspended" {
				continue
			}

			slog.Info("waking a suspended replica for traffic on its address",
				"machine", id, "packets", n-last)
			go func(id string) {
				if err := waker.Wake(context.WithoutCancel(ctx), id); err != nil {
					slog.Error("could not wake a machine traffic arrived for",
						"machine", id, "err", err)
				}
			}(id)
		}

		// Forget machines that no longer have a rule, so a slot reused by a
		// different machine cannot inherit a stale baseline.
		for id := range seen {
			if _, still := counts[id]; !still {
				delete(seen, id)
			}
		}
	}
}

// machineWaker is the one thing the waker needs from the machine layer.
type machineWaker interface {
	Wake(ctx context.Context, id string) error
}

func currentRow(view fleetView, id string) (state.Machine, bool) {
	for _, m := range view.Machines() {
		if m.ID == id {
			return m, true
		}
	}
	return state.Machine{}, false
}
