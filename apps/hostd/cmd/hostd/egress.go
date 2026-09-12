package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// egressInterval is how often the root namespace's egress rules are
// reconciled.
//
// Slower than the tenant filter's two seconds, because the cost of being stale
// is different in kind. A stale tenant filter leaves a new machine unreachable;
// a stale egress table leaves a new machine leaving from the host's shared
// address for a few seconds, which is what it did for the whole of the
// product's life before this existed.
const egressInterval = 10 * time.Second

// runEgress keeps the root namespace's egress table matching the machines this
// host is running, and publishes this host's prefix so that any host can answer
// what address a tenant leaves from.
//
// Runs on EVERY host, including the ones with no egress configuration, which
// is every host until an operator gives one a prefix. It used to return here
// and that was the bug: the masquerade lives in this table, a guest's packets
// reach the root namespace wearing a 10.11 address that is routable nowhere,
// and so the default fleet gave its guests no outbound IPv4 at all. What the
// configuration gates is the per-org REWRITING, where a wrong rule is
// invisible until a tenant's application cannot reach the internet. The
// masquerade is not optional and never was.
func runEgress(ctx context.Context, hostID string, cfg netns.EgressConfig,
	store state.Store, view fleetView, loc *mesh.Locator) {

	if cfg.Enabled() {
		slog.Info("managing per-org egress addresses",
			"interface", cfg.Interface, "prefix", cfg.Prefix6)
		publishEgress(ctx, hostID, cfg, store)
	}

	tick := time.NewTicker(egressInterval)
	defer tick.Stop()

	var applied string
	for {
		// Resolved each tick rather than once at startup: a default route can
		// move, and the masquerade is scoped to an interface name.
		uplink, uerr := netns.UplinkInterface(cfg.Interface)
		plan, err := netns.PlanEgress(cfg.Prefix6, egressBindings(ctx, hostID, store, view, loc))
		switch {
		case uerr != nil:
			slog.Error("could not work out which interface guest traffic leaves by; "+
				"guests have no outbound IPv4 until this host has a default route "+
				"or PILOT_EGRESS_INTERFACE is set", "err", uerr)
		case err != nil:
			slog.Error("could not work out this host's egress rules; running on "+
				"the previous ones", "err", err)
		default:
			if fingerprint := plan.Fingerprint(cfg, uplink); fingerprint != applied {
				if err := netns.ApplyEgress(cfg, plan, uplink); err != nil {
					// Left at the previous fingerprint so the next tick retries,
					// for the reason the tenant filter does the same: a host that
					// silently stopped reconciling keeps rules that get more wrong
					// with every machine that moves.
					slog.Error("could not apply the egress rules; outbound traffic is "+
						"running on the previous ones", "err", err)
				} else {
					applied = fingerprint
					slog.Info("egress reconciled", "uplink", uplink,
						"addresses", len(plan.Addrs), "machines", len(plan.Rules))
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// publishEgress writes this host's prefix so other hosts can answer for it.
//
// Best effort: a host that cannot write the row still applies its own rules,
// and its machines still leave from the right address. What is lost is only
// the ability of ANOTHER host to report that address, which is a reporting gap
// rather than a traffic one, and the next tick of whatever else touches this
// host's rows will not fix it -- so it is logged loudly rather than swallowed.
func publishEgress(ctx context.Context, hostID string, cfg netns.EgressConfig, store state.Store) {
	err := store.PutHostEgress(ctx, &state.HostEgress{
		HostID:    hostID,
		Prefix6:   cfg.Prefix6.String(),
		Interface: cfg.Interface,
		UpdatedAt: time.Now().Unix(),
	})
	if err != nil {
		slog.Error("could not publish this host's egress prefix; its machines will "+
			"still leave from the right address, but no other host can report it",
			"err", err)
	}
}

// egressBindings is every machine this host runs, with the org whose address
// its traffic should leave wearing.
//
// The mesh address is what the root namespace actually sees: a guest's own
// address is the same constant on every machine in the fleet, and the
// namespace has already rewritten it to something unique by the time the
// packet gets here.
func egressBindings(ctx context.Context, hostID string, store state.Store,
	view fleetView, loc *mesh.Locator) []netns.EgressBinding {

	var out []netns.EgressBinding
	for _, m := range view.Machines() {
		if m.HostID != hostID {
			continue
		}
		addr, ok := loc.MachineAddress(m)
		if !ok {
			// Suspended, or not addressable. It is sending nothing.
			continue
		}
		org := ""
		if t, err := store.GetTenancy(ctx, m.ID); err == nil && t != nil {
			org = t.OrgID
		}
		out = append(out, netns.EgressBinding{Machine6: addr, OrgID: org})
	}
	return out
}
