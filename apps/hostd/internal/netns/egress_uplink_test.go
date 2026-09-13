package netns

import (
	"net/netip"
	"strings"
	"testing"
)

// A host that configured no per-org egress still masquerades.
//
// This is the bug these tests exist for. The masquerade lives in the egress
// table, and ApplyEgress used to REMOVE that table on a host with no egress
// configuration -- which is every host until an operator gives one a prefix.
// A guest's packets reach the root namespace wearing the slot's 10.11 address,
// which is routable nowhere, so the default fleet gave its guests no outbound
// IPv4 at all.
//
// It was invisible for the usual reason: the unit tests covered PlanEgress and
// the address derivation, and nothing covered what happens when the feature is
// OFF. The rig found it -- the host could reach 1.1.1.1 and a guest could not.
func TestAnUnconfiguredHostStillNeedsAnUplink(t *testing.T) {
	// With no uplink there is nothing to masquerade out of, and the refusal
	// has to say so rather than install a table that silently does nothing.
	err := ApplyEgress(EgressConfig{}, EgressPlan{}, "")
	if err == nil {
		t.Fatal("ApplyEgress accepted an empty uplink; without one the masquerade " +
			"is scoped to nothing and guests have no outbound IPv4")
	}
	for _, want := range []string{"masquerade", "PILOT_EGRESS_INTERFACE", "default route"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so nobody reading it knows "+
				"what to do: %v", want, err)
		}
	}
}

// The uplink is part of what the reconcile loop compares.
//
// A default route can move. The masquerade is scoped to an interface NAME, so
// a fingerprint blind to the uplink would leave the table pinned to an
// interface the traffic no longer leaves by, and every guest would lose the
// internet with nothing in the log to say why.
func TestTheFingerprintFollowsTheUplink(t *testing.T) {
	cfg := EgressConfig{}
	plan := EgressPlan{}

	if a, b := plan.Fingerprint(cfg, "eth0"), plan.Fingerprint(cfg, "eth1"); a == b {
		t.Fatal("the fingerprint is the same on two different uplinks, so a moved " +
			"default route would never rebuild the table")
	}
	if a, b := plan.Fingerprint(cfg, "eth0"), plan.Fingerprint(cfg, "eth0"); a != b {
		t.Fatal("the fingerprint is unstable for one uplink, so the table would be " +
			"rebuilt on every tick")
	}
}

// A configured uplink is taken as given, because an operator naming an
// interface knows which of a bare-metal host's several is the way out.
func TestAConfiguredInterfaceIsTheUplink(t *testing.T) {
	got, err := UplinkInterface("ens3")
	if err != nil {
		t.Fatalf("UplinkInterface: %v", err)
	}
	if got != "ens3" {
		t.Fatalf("uplink = %q, want the configured ens3", got)
	}
}

// Turning per-org addresses off must stop the REWRITING, which is the half
// that was right, without taking the guests off the internet, which is the
// half that was not.
func TestDisabledMeansNoRewritingNotNoInternet(t *testing.T) {
	off := EgressConfig{}
	if off.Enabled() {
		t.Fatal("an empty config reads as enabled")
	}
	if err := off.Validate(); err != nil {
		t.Fatalf("an empty config must be valid, it is the default: %v", err)
	}

	// And the half that IS gated stays gated: a prefix with no interface is
	// still refused, because a host that rewrites source addresses without
	// being told which way out is a host rewriting traffic between hosts.
	half := EgressConfig{Prefix6: netip.MustParsePrefix("2a01:4f8:1c17:abcd::/64")}
	if err := half.Validate(); err == nil {
		t.Fatal("a prefix with no interface was accepted")
	}
}

// PlanEgress refuses a host with machines and no prefix, which is why the
// reconcile loop must not ask it for one.
//
// This is written down because it is the trap the masquerade fix walked into:
// the loop asked for a plan unconditionally, PlanEgress returned this error
// every tick on any unconfigured host with a machine running, and the apply
// was skipped -- so the masquerade was never installed on exactly the fleet it
// had just been written for. The loop now asks only when per-org egress is
// configured, and this is the behaviour that makes that necessary.
func TestPlanningWithNoPrefixRefusesOnceThereIsAMachine(t *testing.T) {
	binding := EgressBinding{OrgID: "org-1", Machine6: netip.MustParseAddr("fdcd::2")}

	// No machines: nothing to place, so nothing to validate.
	if _, err := PlanEgress(netip.Prefix{}, nil); err != nil {
		t.Fatalf("planning an empty fleet with no prefix failed: %v", err)
	}

	// One machine: the prefix is now load-bearing and its absence is an error.
	if _, err := PlanEgress(netip.Prefix{}, []EgressBinding{binding}); err == nil {
		t.Fatal("planning a machine with no prefix succeeded. If that ever " +
			"becomes true, the loop's reason for only planning when configured " +
			"is gone and this test should be reconsidered rather than deleted")
	}
}
