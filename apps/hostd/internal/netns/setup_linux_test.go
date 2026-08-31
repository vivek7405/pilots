package netns

import (
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
	s := slotForIdx(idx, "pilotstest-"+strings.ReplaceAll(t.Name(), "/", "-"))
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
	s := slotForIdx(903, "pilotstest-never-created")
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

	pool := NewPool(64)
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
