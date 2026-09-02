package mesh

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// The whole point of deriving the address is that nothing has to hand it out.
// A host generates a keypair locally and computes its own address, with no
// registry to ask -- which is what keeps "add a host = give an IP" from
// needing a control plane.
func TestAddressIsDerivedFromTheKeyAlone(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}

	addr := AddressFor(keys.Public)
	if !addr.Is6() {
		t.Fatalf("address %s is not IPv6", addr)
	}
	if !netip.MustParsePrefix("fdcc::/16").Contains(addr) {
		t.Errorf("address %s is outside fdcc::/16", addr)
	}

	// Same key, same address, on any host and at any time.
	if again := AddressFor(keys.Public); again != addr {
		t.Errorf("derivation is not stable: %s then %s", addr, again)
	}

	raw := addr.As16()
	if raw[0] != 0xfd || raw[1] != 0xcc {
		t.Errorf("prefix bytes are %#x %#x, want fd cc", raw[0], raw[1])
	}
	for i := 0; i < 14; i++ {
		if raw[i+2] != keys.Public[i] {
			t.Fatalf("byte %d of the address does not come from the key", i+2)
		}
	}
}

// Two hosts must not land on the same address, or the mesh routes one host's
// traffic to the other.
func TestDistinctKeysGiveDistinctAddresses(t *testing.T) {
	seen := map[netip.Addr]string{}
	for i := 0; i < 200; i++ {
		keys, err := NewKeys()
		if err != nil {
			t.Fatalf("NewKeys: %v", err)
		}
		addr := keys.Address()
		if prev, ok := seen[addr]; ok {
			t.Fatalf("two keys derived the same address %s (%s and %s)",
				addr, prev, keys.Public)
		}
		seen[addr] = keys.Public.String()
	}
}

// A host's identity IS its mesh address, so regenerating the key makes it a
// different host to the fleet -- and every peer's route points at an address
// that no longer answers.
func TestKeysSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.key")

	first, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	second, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateKeys: %v", err)
	}

	if first.Private != second.Private || first.Address() != second.Address() {
		t.Error("the host's identity changed across a restart")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key is mode %o, want 600", perm)
	}
}

// A corrupt key file must fail loudly rather than silently minting a new
// identity, which would move this host's address and strand every peer.
func TestCorruptKeyFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrCreateKeys(path); err == nil {
		t.Error("a corrupt key file was silently replaced with a new identity")
	} else if !strings.Contains(err.Error(), "private key") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

// AllowedIPs is WireGuard's routing table and its inbound filter at once. A
// prefix wider than what the peer's key derives to would let one host claim
// traffic for addresses belonging to others.
func TestPeerIsAllowedOnlyWhatItsKeyDerivesTo(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg := peerConfig(Peer{PublicKey: keys.Public, Address: keys.Address()})

	if len(cfg.AllowedIPs) != 2 {
		t.Fatalf("peer allows %d prefixes, want its own address and its machine block",
			len(cfg.AllowedIPs))
	}
	if !cfg.ReplaceAllowedIPs {
		t.Error("allowed IPs are not replaced, so a stale prefix would survive")
	}

	host, machines := cfg.AllowedIPs[0], cfg.AllowedIPs[1]
	if ones, _ := host.Mask.Size(); ones != 128 {
		t.Errorf("host prefix is /%d, want /128", ones)
	}
	if !host.IP.Equal(keys.Address().AsSlice()) {
		t.Errorf("allowed address %s is not the peer's own %s", host.IP, keys.Address())
	}
	if ones, _ := machines.Mask.Size(); ones != MachinePrefixBits {
		t.Errorf("machine prefix is /%d, want /%d", ones, MachinePrefixBits)
	}
	if !machines.IP.Equal(MachinePrefixFor(keys.Public).Addr().AsSlice()) {
		t.Errorf("machine block %s is not the one the peer's key derives to", machines.IP)
	}
}

// The peer's blocks are derived from its KEY, not read from whatever address
// the row happened to carry. A host that published someone else's address
// would otherwise be handed their traffic.
func TestPeerRoutesIgnoreThePublishedAddress(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	lying := Peer{PublicKey: keys.Public, Address: netip.MustParseAddr("fdcc::dead")}

	routes := peerRoutes(lying)
	if !routes[1].IP.Equal(MachinePrefixFor(keys.Public).Addr().AsSlice()) {
		t.Error("the machine block followed the row rather than the key")
	}
}

// The split between the two spaces is the tenant boundary. A machine address
// that landed in the host space would be reachable by a guest, and hostd's
// internal listener sits there.
func TestMachineAddressesNeverLandInTheHostSpace(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}

	prefix := MachinePrefixFor(keys.Public)
	if !MachineSpace.Contains(prefix.Addr()) {
		t.Fatalf("machine prefix %s is outside %s", prefix, MachineSpace)
	}
	if HostSpace.Contains(prefix.Addr()) {
		t.Fatalf("machine prefix %s overlaps the host space %s", prefix, HostSpace)
	}
	if HostSpace.Contains(AddressFor(keys.Public)) != true {
		t.Fatalf("host address %s left %s", AddressFor(keys.Public), HostSpace)
	}

	for _, slot := range []int{1, 2, 1023, MaxMachineSlot} {
		addr, err := MachineAddr(keys.Public, slot)
		if err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
		if !prefix.Contains(addr) {
			t.Errorf("slot %d derived %s, outside this host's block %s", slot, addr, prefix)
		}
		if HostSpace.Contains(addr) {
			t.Errorf("slot %d derived %s, inside the host space", slot, addr)
		}
	}
}

// Adding the machine block must not move a single host address: that is the
// whole reason for a second prefix rather than one widened one, and it is what
// lets this ship without a flag day.
func TestHostAddressesAreUnchangedByMachineAddressing(t *testing.T) {
	// A fixed key, so this fails if the derivation is ever changed rather than
	// merely if it is inconsistent with itself.
	var key wgtypes.Key
	for i := range key {
		key[i] = byte(i)
	}
	if got, want := AddressFor(key).String(), "fdcc:1:203:405:607:809:a0b:c0d"; got != want {
		t.Errorf("host address derivation changed: got %s, want %s", got, want)
	}
}

// Slot 0 is the netns pool's unallocated sentinel. A zero-valued Slot deriving
// a real-looking address would put an unallocated machine on the mesh.
func TestSlotZeroHasNoAddress(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MachineAddr(keys.Public, 0); err == nil {
		t.Error("slot 0 derived an address")
	}
	if _, err := MachineAddr(keys.Public, MaxMachineSlot+1); err == nil {
		t.Error("a slot beyond the 16-bit suffix derived an address")
	}
}

// Two hosts' machine blocks must not overlap, or one host's guests answer for
// the other's.
func TestDistinctKeysGiveDistinctMachineBlocks(t *testing.T) {
	seen := map[netip.Prefix]bool{}
	for i := 0; i < 200; i++ {
		keys, err := NewKeys()
		if err != nil {
			t.Fatal(err)
		}
		prefix := MachinePrefixFor(keys.Public)
		if seen[prefix] {
			t.Fatalf("two keys derived the same machine block %s", prefix)
		}
		seen[prefix] = true
	}
}

// Reconciliation runs on every hosts-table change, so an unchanged peer must
// be recognised as unchanged. Reconfiguring it anyway tears down a working
// tunnel and drops the gossip and proxied requests riding it.
func TestAnUnchangedPeerIsRecognised(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := Peer{
		PublicKey: keys.Public,
		Address:   keys.Address(),
		Endpoint:  netip.MustParseAddrPort("203.0.113.5:51820"),
	}
	cfg := peerConfig(want)

	have := wgtypes.Peer{
		PublicKey:  cfg.PublicKey,
		AllowedIPs: cfg.AllowedIPs,
		Endpoint:   cfg.Endpoint,
	}
	if !peerMatches(have, want) {
		t.Error("an identical peer was seen as changed; every reconcile would " +
			"rebuild a working tunnel")
	}

	moved := want
	moved.Endpoint = netip.MustParseAddrPort("203.0.113.9:51820")
	if peerMatches(have, moved) {
		t.Error("a peer that moved was seen as unchanged; its tunnel would " +
			"keep pointing at the old address")
	}
}
