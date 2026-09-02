package netns

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// guestNetworkConfig is the systemd-networkd unit baked into the golden
// rootfs. Reached by path because it is not Go: it is the other half of a
// contract this package owns one end of, and the two drifting apart is a
// machine that boots with an address the host does not route.
const guestNetworkConfig = "../../../../scripts/rootfs/eth0.network"

// testPrefix stands in for a real host's derived machine block.
var testPrefix = netip.MustParsePrefix("fdcd:1::/112")

// The high byte of idx lands in the third octet. Formatting these as
// "10.11.0.%d" is correct for idx < 256 and then silently collides -- slot 256
// would alias slot 0. These cases pin the boundaries.
func TestSlotAddressDerivation(t *testing.T) {
	for _, tc := range []struct {
		idx                     int
		hostIP, vethIP, vpeerIP string
	}{
		{1, "10.11.0.1", "10.12.0.2", "10.12.0.3"},
		{2, "10.11.0.2", "10.12.0.4", "10.12.0.5"},
		{127, "10.11.0.127", "10.12.0.254", "10.12.0.255"},
		// veth crosses into the third octet well before the host IP does.
		{128, "10.11.0.128", "10.12.1.0", "10.12.1.1"},
		{255, "10.11.0.255", "10.12.1.254", "10.12.1.255"},
		// The case a "10.11.0.%d" format string gets wrong.
		{256, "10.11.1.0", "10.12.2.0", "10.12.2.1"},
		{511, "10.11.1.255", "10.12.3.254", "10.12.3.255"},
		{512, "10.11.2.0", "10.12.4.0", "10.12.4.1"},
		{1023, "10.11.3.255", "10.12.7.254", "10.12.7.255"},
	} {
		t.Run(fmt.Sprintf("idx=%d", tc.idx), func(t *testing.T) {
			s := slotForIdx(tc.idx, "m", testPrefix)
			if got := s.HostIP.String(); got != tc.hostIP {
				t.Errorf("HostIP = %s, want %s", got, tc.hostIP)
			}
			if got := s.VEthIP.String(); got != tc.vethIP {
				t.Errorf("VEthIP = %s, want %s", got, tc.vethIP)
			}
			if got := s.VPeerIP.String(); got != tc.vpeerIP {
				t.Errorf("VPeerIP = %s, want %s", got, tc.vpeerIP)
			}
		})
	}
}

// Distinct indices must never collide on any address, across the whole pool.
func TestSlotAddressesAreUniqueAcrossPool(t *testing.T) {
	seen := map[string]int{}
	for idx := 1; idx < DefaultPoolSize; idx++ {
		s := slotForIdx(idx, "m", testPrefix)
		for _, addr := range []string{s.HostIP.String(), s.VEthIP.String(), s.VPeerIP.String()} {
			if prev, dup := seen[addr]; dup {
				t.Fatalf("address %s used by both slot %d and slot %d", addr, prev, idx)
			}
			seen[addr] = idx
		}
	}
}

// Every guest sees the same addresses regardless of slot -- this is what makes
// snapshots host-agnostic, so it is worth asserting rather than assuming.
func TestGuestFacingAddressesAreSlotIndependent(t *testing.T) {
	for _, idx := range []int{1, 42, 512, 1023} {
		s := slotForIdx(idx, "m", testPrefix)
		if s.VPeerName != "eth0" {
			t.Errorf("idx %d: VPeerName = %q, want eth0", idx, s.VPeerName)
		}
	}
	if TapGuestIP != "169.254.0.21" || TapHostIP != "169.254.0.22" {
		t.Fatal("guest-facing constants changed: this invalidates every snapshot in the fleet")
	}
	// The v6 pair is guest-facing for exactly the same reason and carries
	// exactly the same consequence. A guest reaches its peers from fdee::21 on
	// every host in the fleet; everything that makes a packet individually
	// routable happens outside the guest.
	if TapGuestIP6 != "fdee::21" || TapHostIP6 != "fdee::22" {
		t.Fatal("guest-facing v6 constants changed: this invalidates every snapshot in the fleet")
	}
}

// Nothing host-specific may enter a snapshot, and the golden rootfs's network
// configuration is the one file where a slot address or a host address could
// be baked in permanently -- it is written once at image build time and then
// captured in the template every machine is created from.
//
// So this reads the real file and asserts it names ONLY constants. The failure
// it guards against does not surface at build time or at create time: it
// surfaces when a machine is restored onto a different host, months later,
// configured for a slot that belongs to someone else.
func TestTheGuestsNetworkConfigNamesOnlyConstants(t *testing.T) {
	raw, err := os.ReadFile(guestNetworkConfig)
	if err != nil {
		t.Skipf("cannot read %s: %v", guestNetworkConfig, err)
	}

	// Comments explain the addressing and therefore mention addresses the
	// configuration must not contain. Only the directives are the contract.
	var directives []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		directives = append(directives, line)
	}
	config := strings.Join(directives, "\n")

	for _, want := range []string{TapGuestIP, TapHostIP, TapGuestIP6, TapHostIP6, "fdcd::/16"} {
		if !strings.Contains(config, want) {
			t.Errorf("the guest's network config does not name %s:\n%s", want, config)
		}
	}

	// hostd listens under fdcc::. A guest that had a route there would be
	// addressing the platform, and the split between the two spaces is the
	// tenant boundary itself.
	if strings.Contains(config, "fdcc") {
		t.Errorf("the guest's network config names the host space:\n%s", config)
	}

	// And nothing derived from a slot or a host key. Sampled across the pool,
	// because the derivation crosses byte boundaries and a mistake at one
	// index need not be visible at another.
	for _, idx := range []int{1, 42, 255, 256, 512, 1023} {
		slot := slotForIdx(idx, "m", testPrefix)
		for _, host := range []string{
			slot.HostIP.String(), slot.VEthIP.String(), slot.VPeerIP.String(),
			slot.VEth6IP.String(), slot.VPeer6IP.String(), slot.Machine6.String(),
			slot.VEthName,
		} {
			if strings.Contains(config, host) {
				t.Errorf("slot %d's address %s is baked into the guest's config; "+
					"this machine could never be restored on another host:\n%s",
					idx, host, config)
			}
		}
	}
}

func TestPoolTakeSkipsZeroAndAdvances(t *testing.T) {
	p := NewPool(8, testPrefix)
	first, err := p.Take("m1")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if first.Idx == 0 {
		t.Fatal("Take handed out index 0, the unallocated sentinel")
	}
	second, err := p.Take("m2")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if second.Idx == first.Idx {
		t.Fatalf("Take returned index %d twice", first.Idx)
	}
	if p.InUse() != 2 {
		t.Errorf("InUse = %d, want 2", p.InUse())
	}
}

func TestPoolExhaustionAndReuse(t *testing.T) {
	p := NewPool(4, testPrefix) // indices 1..3
	var taken []*Slot
	for i := 0; i < 3; i++ {
		s, err := p.Take(fmt.Sprintf("m%d", i))
		if err != nil {
			t.Fatalf("Take %d: %v", i, err)
		}
		taken = append(taken, s)
	}
	if _, err := p.Take("overflow"); err != ErrPoolFull {
		t.Fatalf("got %v, want ErrPoolFull", err)
	}

	p.Return(taken[1].Idx)
	reused, err := p.Take("m-new")
	if err != nil {
		t.Fatalf("Take after Return: %v", err)
	}
	if reused.Idx != taken[1].Idx {
		t.Errorf("reused idx = %d, want the returned %d", reused.Idx, taken[1].Idx)
	}
}

// Reserve is how a restarted hostd re-claims the slots of machines that kept
// running. It must be idempotent, because reconcile can legitimately run
// against a pool that already knows about the slot.
func TestPoolReserveIsIdempotent(t *testing.T) {
	p := NewPool(16, testPrefix)
	s1, err := p.Reserve(7, "m1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	s2, err := p.Reserve(7, "m1")
	if err != nil {
		t.Fatalf("second Reserve for the same machine: %v", err)
	}
	if s1.Idx != s2.Idx || s1.HostIP.String() != s2.HostIP.String() {
		t.Error("Reserve is not deterministic for the same index")
	}
	if p.InUse() != 1 {
		t.Errorf("InUse = %d, want 1 after two Reserves of one slot", p.InUse())
	}
}

func TestPoolReserveRejectsConflictAndOutOfRange(t *testing.T) {
	p := NewPool(16, testPrefix)
	if _, err := p.Reserve(7, "m1"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := p.Reserve(7, "m2"); err == nil {
		t.Error("Reserve let a second machine claim a held slot")
	}
	for _, idx := range []int{0, -1, 16, 99} {
		if _, err := p.Reserve(idx, "m"); err == nil {
			t.Errorf("Reserve(%d) succeeded, want out-of-range error", idx)
		}
	}
}

// Reserve must not hand out an index Take would also hand out.
func TestReserveThenTakeDoNotCollide(t *testing.T) {
	p := NewPool(8, testPrefix)
	reserved, err := p.Reserve(3, "reconciled")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for i := 0; i < 6; i++ {
		s, err := p.Take(fmt.Sprintf("m%d", i))
		if err != nil {
			break
		}
		if s.Idx == reserved.Idx {
			t.Fatalf("Take handed out reserved slot %d", reserved.Idx)
		}
	}
}

// The v6 addressing is derived from the same index as the v4 addressing, and
// the same shift/mask trap applies: a slot beyond 255 must not alias a lower
// one.
func TestSlotIPv6Derivation(t *testing.T) {
	for _, tc := range []struct {
		idx                    int
		veth6, vpeer6, machine string
	}{
		{1, "fdee:1::2", "fdee:1::3", "fdcd:1::1"},
		{2, "fdee:1::4", "fdee:1::5", "fdcd:1::2"},
		{128, "fdee:1::100", "fdee:1::101", "fdcd:1::80"},
		{1023, "fdee:1::7fe", "fdee:1::7ff", "fdcd:1::3ff"},
	} {
		s := slotForIdx(tc.idx, "m", testPrefix)
		if got := s.VEth6IP.String(); got != tc.veth6 {
			t.Errorf("slot %d veth6 = %s, want %s", tc.idx, got, tc.veth6)
		}
		if got := s.VPeer6IP.String(); got != tc.vpeer6 {
			t.Errorf("slot %d vpeer6 = %s, want %s", tc.idx, got, tc.vpeer6)
		}
		if got := s.Machine6.String(); got != tc.machine {
			t.Errorf("slot %d machine = %s, want %s", tc.idx, got, tc.machine)
		}
		if !s.HasMesh() {
			t.Errorf("slot %d has no mesh addressing", tc.idx)
		}
	}
}

// Every slot's veth /127 must be its own. Two slots sharing a link address
// would put two namespaces on one subnet in the root namespace, and the host
// would route one machine's traffic into the other's namespace.
func TestVeth6AddressesDoNotOverlap(t *testing.T) {
	seen := map[string]int{}
	for idx := 1; idx < DefaultPoolSize; idx++ {
		s := slotForIdx(idx, "m", testPrefix)
		for _, addr := range []string{s.VEth6IP.String(), s.VPeer6IP.String(), s.Machine6.String()} {
			if prev, ok := seen[addr]; ok {
				t.Fatalf("slots %d and %d both use %s", prev, idx, addr)
			}
			seen[addr] = idx
		}
	}
}

// A host with no mesh identity has no peer to reach. It must get no IPv6
// rather than half of it: an address with no route and no translation looks
// like a working network and silently blackholes.
func TestASlotWithoutAMeshPrefixHasNoIPv6Machine(t *testing.T) {
	s := slotForIdx(4, "m", netip.Prefix{})
	if s.HasMesh() {
		t.Error("a slot on a host with no mesh identity claims a mesh address")
	}
	if s.Machine6.IsValid() {
		t.Errorf("machine address is %s, want none", s.Machine6)
	}
}
