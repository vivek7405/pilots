package netns

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These exercise the real kernel: namespaces, veth pairs, taps, routes, and
// nftables. They need root, so they skip cleanly for everyone else and run in
// CI and on a developer box under sudo.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates network namespaces")
	}
}

// testSlot keeps indices high to avoid colliding with anything a running hostd
// on the same box may hold.
func testSlot(t *testing.T, idx int) *Slot {
	t.Helper()
	s := slotForIdx(idx, "pilotstest-"+strings.ReplaceAll(t.Name(), "/", "-"), testPrefix)
	t.Cleanup(func() { _ = Teardown(s) })
	return s
}

func nsExists(name string) bool {
	_, err := os.Stat("/var/run/netns/" + name)
	return err == nil
}

func TestSetupTeardownRoundTrip(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 901)

	if err := Setup(s, "02:00:00:00:09:01", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !nsExists(s.NetnsName) {
		t.Fatal("namespace was not created")
	}

	// The guest-facing addressing must be identical on every slot; that is
	// what lets a snapshot restore onto any host.
	out, err := exec.Command("ip", "netns", "exec", s.NetnsName, "ip", "-4", "addr").CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr in ns: %v\n%s", err, out)
	}
	for _, want := range []string{TapHostIP, s.VPeerIP.String(), TapName, "eth0"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("namespace is missing %q:\n%s", want, out)
		}
	}

	// The host route is what makes the slot reachable from the router.
	route, err := exec.Command("ip", "route", "get", s.HostIP.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("ip route get %s: %v\n%s", s.HostIP, err, route)
	}
	if !strings.Contains(string(route), s.VPeerIP.String()) {
		t.Errorf("host route to %s does not go via %s:\n%s", s.HostIP, s.VPeerIP, route)
	}

	if err := Teardown(s); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if nsExists(s.NetnsName) {
		t.Error("namespace survived teardown")
	}
	// Check the exit status, not the output: `ip link show <gone>` prints
	// `Device "<name>" does not exist.` to stderr, so matching on the name
	// would report a survivor either way.
	if err := exec.Command("ip", "link", "show", s.VEthName).Run(); err == nil {
		t.Errorf("host veth %s survived teardown", s.VEthName)
	}
}

// Setup is teardown-first, so a create over a live namespace must succeed
// rather than failing with "file exists". This is the path that broke a
// back-to-back snapshot-and-kill then restore in the predecessor.
func TestSetupIsIdempotent(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 902)

	for attempt := 0; attempt < 3; attempt++ {
		if err := Setup(s, "02:00:00:00:09:02", 0); err != nil {
			t.Fatalf("Setup attempt %d: %v", attempt, err)
		}
		if !nsExists(s.NetnsName) {
			t.Fatalf("attempt %d: namespace missing after Setup", attempt)
		}
	}
}

// Teardown runs on the destroy path AND as Setup's first step, so it must
// tolerate a slot that was never set up.
func TestTeardownOnAbsentSlotIsNoError(t *testing.T) {
	requireRoot(t)
	s := slotForIdx(903, "pilotstest-never-created", testPrefix)
	if err := Teardown(s); err != nil {
		t.Errorf("Teardown of an absent slot: %v", err)
	}
}

// The firewall's value is entirely in its rule ORDER: the slot's own address
// has to be accepted before the 10.0.0.0/8 drop, or the machine is
// unreachable. Assert both the accept and the drop are present and ordered.
func TestFirewallRuleOrdering(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 904)
	if err := Setup(s, "02:00:00:00:09:04", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	out, err := exec.Command("ip", "netns", "exec", s.NetnsName,
		"nft", "list", "table", "inet", "pilots-firewall").CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable in ns: %v\n%s", err, out)
	}
	ruleset := string(out)

	acceptAt := strings.Index(ruleset, s.HostIP.String())
	dropAt := strings.Index(ruleset, "10.0.0.0/8")
	if acceptAt < 0 {
		t.Fatalf("slot address %s is not accepted anywhere:\n%s", s.HostIP, ruleset)
	}
	if dropAt < 0 {
		t.Fatalf("10.0.0.0/8 is not dropped:\n%s", ruleset)
	}
	if acceptAt > dropAt {
		t.Errorf("slot accept comes AFTER the 10/8 drop, so the machine is unreachable:\n%s", ruleset)
	}
	for _, want := range []string{"127.0.0.0/8", "169.254.0.0/16", "192.168.0.0/16", "172.16.0.0/12"} {
		if !strings.Contains(ruleset, want) {
			t.Errorf("firewall does not drop %s:\n%s", want, ruleset)
		}
	}
}

// NAT is a whole-address translation, not a port mapping. Both directions must
// be present or the machine is either unroutable or cannot reach the internet.
func TestNATRulesBothDirections(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 905)
	if err := Setup(s, "02:00:00:00:09:05", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	out, err := exec.Command("ip", "netns", "exec", s.NetnsName,
		"nft", "list", "table", "ip", "pilots-nat").CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable in ns: %v\n%s", err, out)
	}
	ruleset := string(out)
	if !strings.Contains(ruleset, "snat") {
		t.Errorf("no SNAT rule; the guest cannot reach the internet:\n%s", ruleset)
	}
	if !strings.Contains(ruleset, "dnat") {
		t.Errorf("no DNAT rule; the router cannot reach the guest:\n%s", ruleset)
	}
	if !strings.Contains(ruleset, TapGuestIP) {
		t.Errorf("NAT does not reference the constant guest address %s:\n%s", TapGuestIP, ruleset)
	}
}

// Churn is where the EBUSY-on-delete path shows up: a just-killed Firecracker
// holds the namespace fd as a zombie, and swallowing that error leaves a stale
// namespace that breaks the next create.
func TestCreateDestroyChurnLeavesNothingBehind(t *testing.T) {
	requireRoot(t)
	if testing.Short() {
		t.Skip("churn test skipped in -short")
	}

	pool := NewPool(64, netip.MustParsePrefix("fdcd:1::/112"))
	for i := 0; i < 10; i++ {
		slot, err := pool.Take("pilotstest-churn")
		if err != nil {
			t.Fatalf("iteration %d: Take: %v", i, err)
		}
		if err := Setup(slot, "02:00:00:00:0a:01", 0); err != nil {
			t.Fatalf("iteration %d: Setup: %v", i, err)
		}
		if err := Teardown(slot); err != nil {
			t.Fatalf("iteration %d: Teardown: %v", i, err)
		}
		if nsExists(slot.NetnsName) {
			t.Fatalf("iteration %d: namespace leaked", i)
		}
		pool.Return(slot.Idx)
	}
	if pool.InUse() != 0 {
		t.Errorf("slot leak: InUse = %d after balanced take/return", pool.InUse())
	}
}

// The IPv6 half of the namespace is what makes a peer reachable at all, and
// every piece of it has to be there: the guest's gateway, the veth pair the
// translated packet leaves over, and the route the root namespace uses to send
// one back in.
func TestIPv6DataPathIsBuilt(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 906)
	if err := Setup(s, "02:00:00:00:09:06", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	out, err := exec.Command("ip", "netns", "exec", s.NetnsName, "ip", "-6", "addr").CombinedOutput()
	if err != nil {
		t.Fatalf("ip -6 addr in ns: %v\n%s", err, out)
	}
	for _, want := range []string{TapHostIP6, s.VPeer6IP.String()} {
		if !strings.Contains(string(out), want) {
			t.Errorf("namespace is missing the v6 address %q:\n%s", want, out)
		}
	}
	// Tentative means duplicate address detection has not finished, and an
	// address in that state cannot be used. There is nothing to detect on a
	// link with one other node, and waiting a second for it would land inside
	// the create budget.
	if strings.Contains(string(out), "tentative") {
		t.Errorf("a v6 address is still tentative, so it cannot carry traffic yet:\n%s", out)
	}

	// The route that makes the whole design work: the root namespace needs an
	// unambiguous next hop for this machine, which is only possible because
	// the translation happens inside the namespace.
	route, err := exec.Command("ip", "-6", "route", "get", s.Machine6.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("ip -6 route get %s: %v\n%s", s.Machine6, err, route)
	}
	if !strings.Contains(string(route), s.VPeer6IP.String()) {
		t.Errorf("the route to %s does not go via %s:\n%s", s.Machine6, s.VPeer6IP, route)
	}

	// And the namespace forwards, or nothing crosses it.
	fwd, err := exec.Command("ip", "netns", "exec", s.NetnsName,
		"cat", "/proc/sys/net/ipv6/conf/all/forwarding").CombinedOutput()
	if err != nil || strings.TrimSpace(string(fwd)) != "1" {
		t.Errorf("ipv6 forwarding in the namespace is %q (%v)", fwd, err)
	}

	if err := Teardown(s); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	after, err := exec.Command("ip", "-6", "route", "show", s.Machine6.String()+"/128").CombinedOutput()
	if err == nil && strings.Contains(string(after), s.Machine6.String()) {
		t.Errorf("the machine route survived teardown, so the next machine in "+
			"this slot inherits a route to an address that is now someone else's:\n%s", after)
	}
}

// NAT66 is what turns the address every guest shares into one that is
// individually routable. Both directions, or the machine is either unreachable
// or unable to reach anyone.
func TestNAT66RulesBothDirections(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 907)
	if err := Setup(s, "02:00:00:00:09:07", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	out, err := exec.Command("ip", "netns", "exec", s.NetnsName,
		"nft", "list", "table", "ip6", "pilots-nat6").CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable in ns: %v\n%s", err, out)
	}
	ruleset := string(out)
	for _, want := range []string{"snat", "dnat", TapGuestIP6, s.Machine6.String()} {
		if !strings.Contains(ruleset, want) {
			t.Errorf("NAT66 does not mention %q:\n%s", want, ruleset)
		}
	}
}

// fdcc and fdcd differ by one bit. The host space has to be dropped BEFORE the
// machine space is accepted, and both have to come before the blanket fc00::/7
// drop that would otherwise swallow them -- an ordering mistake here silently
// opens hostd's internal listener to every guest on the box.
func TestFirewallOrdersTheTenantBoundary(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 908)
	if err := Setup(s, "02:00:00:00:09:08", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	out, err := exec.Command("ip", "netns", "exec", s.NetnsName,
		"nft", "list", "table", "inet", "pilots-firewall").CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable in ns: %v\n%s", err, out)
	}
	ruleset := string(out)

	hostSpace := strings.Index(ruleset, "fdcc::/16")
	machineSpace := strings.Index(ruleset, "fdcd::/16")
	blanket := strings.Index(ruleset, "fc00::/7")

	if hostSpace < 0 {
		t.Fatalf("the host space is not dropped explicitly:\n%s", ruleset)
	}
	if machineSpace < 0 {
		t.Fatalf("the machine space is not accepted, so no peer is reachable:\n%s", ruleset)
	}
	if blanket < 0 {
		t.Fatalf("fc00::/7 is no longer dropped:\n%s", ruleset)
	}
	if !(hostSpace < machineSpace && machineSpace < blanket) {
		t.Errorf("the tenant boundary is out of order (fdcc %d, fdcd %d, fc00 %d):\n%s",
			hostSpace, machineSpace, blanket, ruleset)
	}
	// And the guest's own link stays reachable, or neighbour discovery
	// replies -- which arrive as unicast to these addresses -- are dropped and
	// v6 fails in a way that looks like packet loss.
	if !strings.Contains(ruleset, "fdee::20/126") {
		t.Errorf("the guest's v6 link is not accepted:\n%s", ruleset)
	}
}

// Neighbour discovery must survive the link-local drop.
//
// The regression this pins cost a day. The namespace resolves the root
// namespace's veth address by soliciting it, and the kernel sources that
// solicitation from the interface's LINK-LOCAL address -- so the reply comes
// back addressed to fe80::, which the fe80::/10 drop then eats. The neighbour
// entry stays FAILED forever, every guest packet bound for the mesh is dropped
// locally for want of a next hop, and the symptom is 100% loss on a host where
// the routes, the NAT rules and the tenant sets are all provably correct. The
// accepts for fdee::/126 above do NOT cover this: they match the addresses NDP
// was assumed to use, not the one the kernel actually picks.
func TestNeighbourDiscoverySurvivesTheLinkLocalDrop(t *testing.T) {
	requireRoot(t)
	s := testSlot(t, 909)
	if err := Setup(s, "02:00:00:00:09:09", 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	out, err := exec.Command("ip", "netns", "exec", s.NetnsName,
		"nft", "list", "table", "inet", "pilots-firewall").CombinedOutput()
	if err != nil {
		t.Skipf("nft unavailable in ns: %v\n%s", err, out)
	}
	ruleset := string(out)

	linkLocal := strings.Index(ruleset, "fe80::/10")
	if linkLocal < 0 {
		t.Fatalf("fe80::/10 is no longer dropped:\n%s", ruleset)
	}
	for _, nd := range []string{"nd-neighbor-solicit", "nd-neighbor-advert"} {
		at := strings.Index(ruleset, nd)
		if at < 0 {
			t.Errorf("%s is not accepted; the namespace cannot resolve its "+
				"next hop and v6 egress fails as silent loss:\n%s", nd, ruleset)
			continue
		}
		if at > linkLocal {
			t.Errorf("%s is accepted at %d, AFTER the fe80::/10 drop at %d, "+
				"so the drop wins and the accept is dead:\n%s",
				nd, at, linkLocal, ruleset)
		}
	}

	// Router advertisement stays dropped: the namespace's routes are
	// configured, not discovered, so a guest able to inject one would be
	// reconfiguring the host's side of the link.
	if strings.Contains(ruleset, "nd-router-advert") {
		t.Errorf("router advertisement is accepted; a guest can reconfigure "+
			"the link:\n%s", ruleset)
	}
}
