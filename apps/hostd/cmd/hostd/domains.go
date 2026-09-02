package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// verifyInterval is how often unverified custom domains are re-checked.
//
// DNS propagation is the thing being waited on and it is measured in minutes,
// so checking faster only makes more queries. Slower would leave a customer
// who set their CNAME correctly staring at a TLS error for no reason.
const verifyInterval = time.Minute

// runDomainVerifier re-checks custom hostnames that have not verified yet.
//
// Registration checks once, and a customer almost always registers BEFORE the
// CNAME propagates -- that is the natural order, since the target is only
// known once the service exists. Without this loop that first check is the
// only one there will ever be: the row sits at verified_at = 0, certs.Decider
// skips it, no certificate is ever issued, and TLS fails permanently for a
// domain whose DNS is correct. The handler's own 202 promises "the next
// verification pass"; this is it.
//
// Only the service's arbiter acts, for the same reason it is the only writer
// of the row.
func runDomainVerifier(ctx context.Context, hostID string, store state.Store, domain string) {
	tick := time.NewTicker(verifyInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		domains, err := store.ListDomains(ctx)
		if err != nil {
			continue
		}
		hosts, err := store.ListHosts(ctx)
		if err != nil {
			continue
		}
		live := state.LiveHosts(hosts)

		for i := range domains {
			d := domains[i]
			if d.VerifiedAt != 0 {
				continue
			}
			if owner, ok := state.OwnerFor(d.ServiceID, live); ok && owner != hostID {
				continue
			}
			svc, err := store.GetService(ctx, d.ServiceID)
			if err != nil {
				continue
			}
			if err := api.VerifyHostname(ctx, nil, d.Hostname, svc.Domain+"."+domain); err != nil {
				continue
			}
			d.VerifiedAt = time.Now().Unix()
			if err := store.PutDomain(ctx, &d); err != nil {
				slog.Warn("could not record a verified domain",
					"hostname", d.Hostname, "err", err)
				continue
			}
			slog.Info("custom domain verified; certificates can now be issued for it",
				"hostname", d.Hostname, "service", d.ServiceID)
		}
	}
}
