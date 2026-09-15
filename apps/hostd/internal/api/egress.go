package api

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The addresses a tenant's outbound traffic leaves from.
//
// A tenant integrating with anything that allowlists by source address needs
// to know what to give them. That is what this route is for, and it is one
// route rather than a field on a machine because the answer is per HOST: an
// org's address is derived from the host's own prefix, so an org running
// machines on three hosts leaves from three addresses.
//
// The set changes only when a host joins or leaves the fleet. It does not
// change when the tenant's machines are created, destroyed, resized, rolled or
// moved, which is the whole reason the address is per org rather than per
// machine.

// handleEgress reports every address the acting org's traffic can leave from.
func (d Deps) handleEgress(w http.ResponseWriter, r *http.Request) {
	org := actingOrg(r)
	if org == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"an egress address belongs to an org, and this key names none",
			"use an org-scoped key", nil)
		return
	}
	rows, err := d.Store.ListHostEgress(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	out := EgressResponse{OrgID: org, Addresses: []EgressAddress{}}
	for _, row := range rows {
		addr, ok := egressAddrOf(row, org)
		if !ok {
			continue
		}
		out.Addresses = append(out.Addresses, EgressAddress{
			HostID: row.HostID, IPv6: addr.String(), Interface: row.Interface,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// egressAddrOf is the org's address on one host, or false when that host
// manages no egress or its row cannot be read as a prefix.
//
// A bad row is skipped rather than reported as an address. Handing a tenant an
// address to put in somebody else's firewall is a promise; one derived from a
// prefix that does not parse is a promise this host cannot keep, and silence
// is the honest answer.
func egressAddrOf(row state.HostEgress, orgID string) (netip.Addr, bool) {
	prefix, err := netip.ParsePrefix(row.Prefix6)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netns.OrgAddr6(prefix, orgID)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// egressOf is the address a single machine's traffic leaves from, for the
// machine read. Empty when its host manages no egress, which is what every
// host did before egress addresses existed.
func (d Deps) egressOf(ctx context.Context, hostID, orgID string) string {
	if hostID == "" || orgID == "" {
		return ""
	}
	row, err := d.Store.GetHostEgress(ctx, hostID)
	if err != nil || row == nil {
		return ""
	}
	addr, ok := egressAddrOf(*row, orgID)
	if !ok {
		return ""
	}
	return addr.String()
}
