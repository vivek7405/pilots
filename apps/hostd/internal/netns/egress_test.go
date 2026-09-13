package netns

import (
	"net/netip"
	"testing"
)

const testEgressPrefix = "2a01:4f8:1c17:abcd::/64"

func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", s, err)
	}
	return p
}

// The whole value of the address is that a tenant can put it in somebody
// else's firewall. That is only true if it never moves: not across restarts,
// not across hosts computing it independently, not when the org's machines
// change.
func TestOrgAddrIsTheSameEveryTime(t *testing.T) {
	p := prefix(t, testEgressPrefix)

	first, err := OrgAddr6(p, "org_acme")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := OrgAddr6(p, "org_acme")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("run %d gave %s, first gave %s", i, again, first)
		}
	}
	// And it is not the same for a different org, which is the other half of
	// being useful: two tenants allowlisted separately must be separable.
	other, err := OrgAddr6(p, "org_other")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Errorf("two orgs share %s; one tenant would inherit the other's access", other)
	}
}

// An address outside the prefix is not routed to this host, so traffic leaving
// from it never comes back. That failure is silent and looks like the remote
// end dropping the connection.
func TestOrgAddrIsInsideThePrefix(t *testing.T) {
	p := prefix(t, testEgressPrefix)

	for _, org := range []string{"org_1", "org_2", "a", "org_with_a_very_long_identifier_indeed", "ORG"} {
		addr, err := OrgAddr6(p, org)
		if err != nil {
			t.Fatalf("%s: %v", org, err)
		}
		if !p.Contains(addr) {
			t.Errorf("%s -> %s, which is outside %s", org, addr, p)
		}
		if addr == p.Masked().Addr() {
			t.Errorf("%s took the subnet-router anycast address %s", org, addr)
		}
		// The EUI-64 range, which autoconfiguration may claim.
		raw := addr.As16()
		if raw[11] == 0xff && raw[12] == 0xfe {
			t.Errorf("%s -> %s, inside the range reserved for EUI-64 autoconfiguration", org, addr)
		}
	}
}

// A prefix a host cannot actually route is refused at startup rather than
// producing addresses the world has never heard of.
func TestABadPrefixIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{
		{"IPv4", "203.0.113.0/24"},
		{"too long to hash into", "2a01:4f8:1c17:abcd::/112"},
		{"shorter than a routed block", "2a01:4f8::/48"},
		{"a private ULA nothing routes", "fd00:dead:beef::/64"},
		{"link local", "fe80::/64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prefix(t, tc.prefix)
			if err := ValidEgressPrefix(p); err == nil {
				t.Errorf("%s was accepted", tc.prefix)
			}
			if _, err := OrgAddr6(p, "org_1"); err == nil {
				t.Errorf("%s produced an address anyway", tc.prefix)
			}
		})
	}
	// And the one that should work, so the test above cannot pass by refusing
	// everything.
	if err := ValidEgressPrefix(prefix(t, testEgressPrefix)); err != nil {
		t.Errorf("a routed /64 was refused: %v", err)
	}
}

// One address per ORG, however many machines that org is running here. The
// rules are per machine because the root namespace sees a machine's mesh
// address and nothing about who owns it.
func TestPlanCollapsesAnOrgsMachinesOntoOneAddress(t *testing.T) {
	p := prefix(t, testEgressPrefix)
	plan, err := PlanEgress(p, []EgressBinding{
		{Machine6: netip.MustParseAddr("fdaa::1"), OrgID: "org_a"},
		{Machine6: netip.MustParseAddr("fdaa::2"), OrgID: "org_a"},
		{Machine6: netip.MustParseAddr("fdaa::3"), OrgID: "org_b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Addrs) != 2 {
		t.Errorf("%d addresses on the uplink, want one per org: %v", len(plan.Addrs), plan.Addrs)
	}
	if len(plan.Rules) != 3 {
		t.Errorf("%d rules, want one per machine", len(plan.Rules))
	}
	if plan.Rules[0].To != plan.Rules[1].To {
		t.Error("two machines of one org leave from different addresses")
	}
	if plan.Rules[1].To == plan.Rules[2].To {
		t.Error("two orgs leave from the same address")
	}
	// Every rule's target must be an address the uplink is actually given, or
	// the rewrite sends replies nowhere.
	on := map[netip.Addr]bool{}
	for _, a := range plan.Addrs {
		on[a] = true
	}
	for _, r := range plan.Rules {
		if !on[r.To] {
			t.Errorf("rule for %s rewrites to %s, which is on no interface", r.Machine6, r.To)
		}
	}
}

// A machine whose owner is not known keeps the host's shared address, which is
// what it has always had. Giving it some default org's address would put one
// tenant's traffic behind another tenant's allowlisted address -- the one
// outcome here worse than not having the feature.
func TestAMachineWithNoOrgGetsNoRule(t *testing.T) {
	p := prefix(t, testEgressPrefix)
	plan, err := PlanEgress(p, []EgressBinding{
		{Machine6: netip.MustParseAddr("fdaa::1"), OrgID: ""},
		{Machine6: netip.MustParseAddr("fdaa::2"), OrgID: "org_a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rules) != 1 || plan.Rules[0].OrgID != "org_a" {
		t.Errorf("rules = %+v, want only org_a's", plan.Rules)
	}
}

// The counterfactual for the whole feature: a host told nothing about egress
// installs nothing, so every existing fleet keeps the behaviour it has.
func TestNoMachinesMeansNoPlan(t *testing.T) {
	plan, err := PlanEgress(netip.Prefix{}, nil)
	if err != nil {
		t.Fatalf("an unconfigured host must not fail: %v", err)
	}
	if len(plan.Addrs) != 0 || len(plan.Rules) != 0 {
		t.Errorf("an unconfigured host planned %+v", plan)
	}
}

// The plan is a value, so two runs over the same fleet must produce the same
// table. Without this the reconcile could not tell "nothing changed" from "the
// map iterated in another order", and would rewrite the root namespace's rules
// every tick.
func TestThePlanIsStable(t *testing.T) {
	p := prefix(t, testEgressPrefix)
	in := []EgressBinding{
		{Machine6: netip.MustParseAddr("fdaa::9"), OrgID: "org_b"},
		{Machine6: netip.MustParseAddr("fdaa::1"), OrgID: "org_a"},
		{Machine6: netip.MustParseAddr("fdaa::5"), OrgID: "org_c"},
	}
	first, err := PlanEgress(p, in)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := []EgressBinding{in[2], in[0], in[1]}
	again, err := PlanEgress(p, shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rules) != len(again.Rules) {
		t.Fatalf("%d rules then %d", len(first.Rules), len(again.Rules))
	}
	for i := range first.Rules {
		if first.Rules[i] != again.Rules[i] {
			t.Errorf("rule %d: %+v then %+v", i, first.Rules[i], again.Rules[i])
		}
	}
	for i := range first.Addrs {
		if first.Addrs[i] != again.Addrs[i] {
			t.Errorf("address %d: %s then %s", i, first.Addrs[i], again.Addrs[i])
		}
	}
}
