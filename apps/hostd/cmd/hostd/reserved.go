package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// shadowCheckInterval is how often the fleet is re-read for a shadowed URL.
//
// Slow on purpose. The condition it looks for is created by a machine that
// already existed before the control API moved onto its hostname, and it ends
// only when an operator destroys that machine -- so this is a fleet-state read
// on a five-minute clock, not a hot path.
const shadowCheckInterval = 5 * time.Minute

// runAPIHostnameShadowCheck says out loud when a machine's own URL is being
// answered by the control API.
//
// dispatch claims the API hostname before the workload suffix, and
// ensureNotReserved keeps a tenant from taking the name that produces it. But
// that guard only runs on create: a machine that took the name BEFORE the
// control API was pointed at it keeps its row and its domain, and from that
// deploy on every request for its URL is answered by the control API instead
// of being routed to it. The machine is still running, still billed, and
// unreachable at the URL it was given -- which is invariant 4, URLs are
// permanent, failing in the one direction nothing else notices.
//
// Loud rather than fatal. Refusing to start would be the wrong trade twice
// over: the offending row is fleet-wide, so EVERY host would refuse over one
// tenant's name and one machine's lost URL would become a fleet outage, and it
// would put a store read on the boot path of the data plane, which is exactly
// the dependency ARCHITECTURE.md forbids. So this logs an error naming the
// machine and publishes a gauge an operator can alert on, and the host serves.
//
// The pass repeats because the condition clears: an operator who destroys the
// machine should see the gauge fall and the log say so, without restarting
// hostd to find out. Logging is edge-triggered so a permanent condition is not
// a line every five minutes.
func runAPIHostnameShadowCheck(ctx context.Context, store state.Store, apiHostname string) {
	if apiHostname == "" {
		return
	}
	var last string
	for {
		machines, err := store.ListMachines(ctx)
		if err == nil {
			last = reportShadowed(apiHostnameShadowed(machines, apiHostname), apiHostname, last)
		} else if ctx.Err() == nil {
			slog.Warn("could not check whether a machine's URL is shadowed by "+
				"the control API hostname", "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(shadowCheckInterval):
		}
	}
}

// apiHostnameShadowed returns the machines whose URL the control API answers.
//
// Both the assigned domain and a custom one, because dispatch compares the
// request's Host against the API hostname before it looks at anything else --
// a custom domain pointed at it is swallowed the same way. A destroyed machine
// is not shadowed; it has no URL to lose.
func apiHostnameShadowed(machines []state.Machine, apiHostname string) []state.Machine {
	var out []state.Machine
	for _, m := range machines {
		if m.State == state.StateDestroyed {
			continue
		}
		if strings.EqualFold(m.Domain, apiHostname) ||
			strings.EqualFold(m.CustomDomain, apiHostname) {
			out = append(out, m)
		}
	}
	return out
}

// reportShadowed publishes the count and logs the change. It returns the key
// of what it saw, which the caller feeds back in so an unchanged condition is
// not logged again.
func reportShadowed(shadowed []state.Machine, apiHostname, last string) string {
	metrics.APIHostnameShadowed.Set(int64(len(shadowed)))

	ids := make([]string, 0, len(shadowed))
	for _, m := range shadowed {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	key := strings.Join(ids, ",")
	if key == last {
		return key
	}

	if key == "" {
		slog.Info("no machine's URL is shadowed by the control API hostname any more",
			"api_hostname", apiHostname)
		return key
	}
	for _, m := range shadowed {
		slog.Error("this machine is UNREACHABLE at its own URL: the control API "+
			"answers on that hostname, so requests for it never reach the "+
			"machine. Destroy or recreate it under another name.",
			"machine", m.ID, "name", m.Name, "host", m.HostID,
			"hostname", apiHostname)
	}
	return key
}
