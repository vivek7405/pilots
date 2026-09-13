package netns

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
)

// The address a tenant's outbound traffic leaves from.
//
// # The problem
//
// A guest's packets leave this host wearing the host's address, which is
// shared by every tenant on it. A tenant integrating with anything that
// allowlists by source address -- a payment processor, a partner's API, a
// database behind a firewall -- has nothing to give them. That is not a
// nice-to-have: it is the reason the integration cannot be done at all, and
// every platform this is measured against answers it.
//
// # Why IPv6, and why per ORG rather than per machine
//
// An IPv4 address is a scarce, purchased resource. A bare-metal host has one,
// sometimes a handful, and handing one to every tenant does not scale past the
// first few. An IPv6 prefix is routed to the host at no cost and holds more
// addresses than there will ever be tenants, so the per-tenant address is v6
// and the v4 path stays a shared masquerade. That is the honest shape rather
// than a promise that runs out.
//
// Per org rather than per machine because the allowlist entry has to survive
// the machine. A tenant's machines are created, destroyed, resized, rolled and
// moved between hosts constantly; an address that changed with any of that
// would have to be re-allowlisted, which is the opposite of the point.
//
// # Why it is derived and not allocated
//
// There is no allocator and no table of assignments, because there is nothing
// to allocate: the address is a pure function of the host's prefix and the
// org's id. Two hosts compute the same answer without talking, a host that has
// just booted needs no state to answer, and there is no row that can drift
// from the rule that produced it. A /64 gives 2^64 addresses to hash into, so
// a collision between two orgs on one host is not a risk worth a coordinator.
//
// The consequence, stated rather than discovered: an org's address is
// DIFFERENT on each host, because each host has its own prefix. A tenant that
// needs to allowlist gets the whole set, one per host, from GET /v1/egress --
// and the set changes only when a host joins or leaves the fleet, not when
// their machines move.

// EgressPrefixBits is the prefix a host must be given for per-org addresses.
//
// A /64 is what a provider routes to a machine by default and is the smallest
// prefix with enough room that hashing into it needs no collision handling.
// Anything longer is refused rather than silently made to work, because a /112
// would collide between orgs often enough to hand two tenants one address --
// which looks like it works right up to the moment one of them is allowlisted
// and the other inherits the access.
const EgressPrefixBits = 64

// OrgAddr6 is the address an org's traffic leaves this host from.
//
// Deterministic: the same prefix and org always give the same address, on
// every host and across restarts, which is what makes it safe to put in
// somebody else's firewall.
func OrgAddr6(prefix netip.Prefix, orgID string) (netip.Addr, error) {
	if err := ValidEgressPrefix(prefix); err != nil {
		return netip.Addr{}, err
	}
	if orgID == "" {
		return netip.Addr{}, fmt.Errorf("netns: an egress address needs an org")
	}

	sum := sha256.Sum256([]byte("pilots-egress\x00" + orgID))
	host := binary.BigEndian.Uint64(sum[:8])

	// Two addresses in a /64 are not ours to hand out: ::0 is the subnet
	// router anycast address, and the range ending ...ff:fe00:0 through
	// ...ff:feff:ffff is reserved for EUI-64 autoconfiguration. Both would
	// work today and break the day something on the link claims them, so the
	// hash is folded away from them instead.
	if host == 0 {
		host = 1
	}
	if host>>32 == 0x0000_00ff && (host>>24)&0xff == 0xfe {
		host ^= 1 << 40
	}

	raw := prefix.Masked().Addr().As16()
	binary.BigEndian.PutUint64(raw[8:], host)
	return netip.AddrFrom16(raw), nil
}

// ValidEgressPrefix is what a host's configured prefix must be. Checked at
// startup, so a typo is a refused boot rather than a fleet quietly handing out
// addresses nobody routes.
func ValidEgressPrefix(prefix netip.Prefix) error {
	if !prefix.IsValid() {
		return fmt.Errorf("netns: the egress prefix is not an address block")
	}
	if !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
		return fmt.Errorf("netns: the egress prefix %s is not IPv6; a per-tenant "+
			"address is v6 because a v4 one cannot be given to every tenant", prefix)
	}
	if prefix.Bits() != EgressPrefixBits {
		return fmt.Errorf("netns: the egress prefix %s is a /%d; it must be a /%d, "+
			"which is what a provider routes and what leaves room to hash into",
			prefix, prefix.Bits(), EgressPrefixBits)
	}
	if !prefix.Addr().IsGlobalUnicast() || prefix.Addr().IsPrivate() {
		return fmt.Errorf("netns: the egress prefix %s is not globally routable, "+
			"so traffic leaving from it would never come back", prefix)
	}
	return nil
}

// EgressBinding is one machine's outbound identity on this host: the mesh
// address its packets carry when they reach the root namespace, and the org
// whose address they should leave wearing.
type EgressBinding struct {
	Machine6 netip.Addr
	OrgID    string
}

// EgressPlan is what the root namespace must be told: which addresses to put
// on the uplink, and which source rewrites to install.
//
// Computed as a pure value and applied separately, so what the rules WILL be
// is testable without a host, a namespace or root.
type EgressPlan struct {
	// Addrs are the /128s to assign to the uplink, sorted. Without these the
	// SNAT would rewrite the source of packets that then have nowhere to come
	// back to, which looks like a working rule and a network that eats every
	// reply.
	Addrs []netip.Addr
	// Rules are the source rewrites, sorted by machine address so the plan is
	// stable and two runs over the same fleet produce the same table.
	Rules []EgressRule
}

// EgressRule is one rewrite: traffic from this machine leaves as this address.
type EgressRule struct {
	Machine6 netip.Addr
	To       netip.Addr
	OrgID    string
}

// PlanEgress works out the addresses and rules for a set of machines.
//
// A machine whose org is unknown gets NO rule, rather than a rule to some
// default address. Leaving it on the host's shared address is the behaviour it
// has always had; inventing an org for it would put one tenant's traffic
// behind another tenant's allowlisted address, which is the one failure here
// that is worse than the feature being absent.
func PlanEgress(prefix netip.Prefix, bindings []EgressBinding) (EgressPlan, error) {
	var plan EgressPlan
	if len(bindings) == 0 {
		return plan, nil
	}
	if err := ValidEgressPrefix(prefix); err != nil {
		return plan, err
	}

	seen := map[netip.Addr]bool{}
	for _, b := range bindings {
		if b.OrgID == "" || !b.Machine6.IsValid() {
			continue
		}
		to, err := OrgAddr6(prefix, b.OrgID)
		if err != nil {
			return EgressPlan{}, err
		}
		plan.Rules = append(plan.Rules, EgressRule{Machine6: b.Machine6, To: to, OrgID: b.OrgID})
		if !seen[to] {
			seen[to] = true
			plan.Addrs = append(plan.Addrs, to)
		}
	}
	sort.Slice(plan.Rules, func(i, j int) bool {
		return plan.Rules[i].Machine6.Less(plan.Rules[j].Machine6)
	})
	sort.Slice(plan.Addrs, func(i, j int) bool { return plan.Addrs[i].Less(plan.Addrs[j]) })
	return plan, nil
}
