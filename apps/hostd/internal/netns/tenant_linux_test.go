package netns

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requirePrivateNetns refuses to run in the host's own network namespace.
//
// ApplyTenantFilter writes ROOT-namespace nftables rules and turns on IPv6
// forwarding. Run on a developer box under plain sudo that is the developer's
// box, so this asks to be run under `unshare -n` instead:
//
//	sudo unshare -n -m --propagation private go test ./internal/netns/
func requirePrivateNetns(t *testing.T) {
	t.Helper()
	requireRoot(t)

	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Skipf("cannot read this process's network namespace: %v", err)
	}
	host, err := os.Readlink("/proc/1/ns/net")
	if err == nil && self == host {
		t.Skip("this writes root-namespace nftables rules; run under `unshare -n`")
	}
}

func tenantFixture() TenantRules {
	shop := []netip.Addr{
		netip.MustParseAddr("fdcd:1::7"),
		netip.MustParseAddr("fdcd:2::9"),
	}
	return TenantRules{
		Local: []TenantMachine{
			{SlotIdx: 7, Addr: shop[0], App: "shop"},
			// A machine in no app: reachable from nowhere and able to reach
			// nothing, which is the right default for a sandbox nobody grouped.
			{SlotIdx: 8, Addr: netip.MustParseAddr("fdcd:1::8")},
		},
		Apps: map[string][]netip.Addr{"shop": shop},
	}
}

// The rules only mean anything if the kernel takes them. A malformed
// expression is not a compile error and not a test failure anywhere else --
// it is a Flush that fails on a real host, at which point guests can reach
// each other with no filter at all.
func TestApplyTenantFilterLoadsIntoTheKernel(t *testing.T) {
	requirePrivateNetns(t)

	if err := ApplyTenantFilter(tenantFixture()); err != nil {
		t.Fatalf("ApplyTenantFilter: %v", err)
	}

	out, err := exec.Command("nft", "list", "table", "inet", tenantTable).CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable: %v\n%s", err, out)
	}
	ruleset := string(out)

	for _, want := range []string{
		// Classification is by the interface a packet arrived on, because
		// every guest shares one source address and nothing in a packet
		// identifies its sender.
		`iifname "veth-7"`,
		`oifname "veth-7"`,
		`iifname "veth-8"`,
		appSetName("shop"),
		"fdcd:1::7",
		"drop",
		"accept",
	} {
		if !strings.Contains(ruleset, want) {
			t.Errorf("the tenant filter does not contain %q:\n%s", want, ruleset)
		}
	}

	// The ungrouped machine gets no set to be checked against, so its rules
	// are a bare drop. If a set name appeared on its veth it would have been
	// given somebody's app.
	for _, line := range strings.Split(ruleset, "\n") {
		if strings.Contains(line, `iifname "veth-8"`) && strings.Contains(line, "@app-") {
			t.Errorf("a machine in no app was given an app set: %s", line)
		}
	}
}

// Applying twice must land in the same place. The loop that drives this runs
// every couple of seconds, and a second pass that duplicated or stacked rules
// would grow the ruleset without bound on a host that never changes.
func TestApplyTenantFilterIsIdempotent(t *testing.T) {
	requirePrivateNetns(t)

	rules := tenantFixture()
	if err := ApplyTenantFilter(rules); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, err := exec.Command("nft", "list", "table", "inet", tenantTable).CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable: %v\n%s", err, first)
	}

	if err := ApplyTenantFilter(rules); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second, err := exec.Command("nft", "list", "table", "inet", tenantTable).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("the ruleset changed on a second apply of identical state:\n%s\n---\n%s",
			first, second)
	}
}

// A machine that goes away must stop being reachable. The rules are rebuilt
// rather than diffed, so this is really asserting that the previous
// generation is removed in the same transaction as the new one is added.
func TestApplyTenantFilterForgetsAMachineThatLeft(t *testing.T) {
	requirePrivateNetns(t)

	if err := ApplyTenantFilter(tenantFixture()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := ApplyTenantFilter(TenantRules{Apps: map[string][]netip.Addr{}}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	out, err := exec.Command("nft", "list", "table", "inet", tenantTable).CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "veth-7") {
		t.Errorf("a machine that left the host still has rules:\n%s", out)
	}
}

// The activity chain is the one rule set nothing else proves. A malformed
// counted pass-through is not a compile error and shows up nowhere but here:
// on a real host the Flush fails and the whole tenant filter goes with it.
func TestActivityRulesLoadIntoTheKernel(t *testing.T) {
	requirePrivateNetns(t)

	rules := tenantFixture()
	rules.Activity = []WakeTarget{
		{MachineID: "m-7", Addr: netip.MustParseAddr("fdcd:1::7")},
	}
	if err := ApplyTenantFilter(rules); err != nil {
		t.Fatalf("ApplyTenantFilter: %v", err)
	}

	out, err := exec.Command("nft", "list", "table", "inet", tenantTable).CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable: %v\n%s", err, out)
	}
	ruleset := string(out)

	if !strings.Contains(ruleset, "chain "+activityChain) {
		t.Fatalf("the activity chain did not load:\n%s", ruleset)
	}
	chain := chainBody(ruleset, activityChain)
	if !strings.Contains(chain, "fdcd:1::7") || !strings.Contains(chain, "counter") {
		t.Errorf("no counted rule for the running replica:\n%s", chain)
	}
	// A verdict here would either drop a peer's traffic or accept it past the
	// tenant boundary. The rule counts and lets evaluation continue.
	if strings.Contains(chain, "drop") || strings.Contains(chain, "accept") {
		t.Errorf("the activity chain decides something:\n%s", chain)
	}
}

// chainBody returns the RULE lines of one chain out of an `nft list table`
// dump. The chain's own declaration is left out: its policy is an accept and
// the caller is asking about verdicts the rules carry.
func chainBody(ruleset, name string) string {
	var out []string
	var in bool
	for _, line := range strings.Split(ruleset, "\n") {
		switch {
		case strings.Contains(line, "chain "+name+" {"):
			in = true
		case in && strings.TrimSpace(line) == "}":
			return strings.Join(out, "\n")
		case in && strings.Contains(line, "policy "):
			// the chain declaration, not a rule
		case in:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
