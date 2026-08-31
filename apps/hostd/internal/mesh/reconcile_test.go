package mesh

import (
	"net/netip"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func TestPeersFromSkipsSelfAndKeylessHosts(t *testing.T) {
	self, _ := NewKeys()
	other, _ := NewKeys()

	peers := PeersFrom([]state.Host{
		{ID: "host-a", WGPubKey: self.Public.String(), PublicIP: "203.0.113.1"},
		{ID: "host-b", WGPubKey: other.Public.String(), PublicIP: "203.0.113.2"},
		{ID: "host-c"}, // joined but has not published a key yet
	}, "host-a")

	if len(peers) != 1 {
		t.Fatalf("got %d peers, want only host-b: %+v", len(peers), peers)
	}
	if peers[0].PublicKey != other.Public {
		t.Error("the wrong host became a peer")
	}
	if peers[0].Endpoint != netip.MustParseAddrPort("203.0.113.2:51820") {
		t.Errorf("endpoint = %s", peers[0].Endpoint)
	}
}

// A row's wg_addr is a convenience for operators reading the table. Routing
// off it would let a host publish someone else's address and take over their
// traffic, since nothing validates a row against anything but its writer.
func TestPeerAddressIsDerivedNotReadFromTheRow(t *testing.T) {
	victim, _ := NewKeys()
	attacker, _ := NewKeys()

	peers := PeersFrom([]state.Host{{
		ID:       "host-evil",
		WGPubKey: attacker.Public.String(),
		// Claiming the victim's address.
		WGAddr:   victim.Address().String(),
		PublicIP: "203.0.113.9",
	}}, "host-a")

	if len(peers) != 1 {
		t.Fatalf("got %d peers", len(peers))
	}
	if peers[0].Address == victim.Address() {
		t.Fatal("a host took over another's mesh address by claiming it in its row")
	}
	if peers[0].Address != AddressFor(attacker.Public) {
		t.Errorf("address %s is not the one the key derives to", peers[0].Address)
	}
}

// A row that cannot be parsed must cost that one host, not the whole mesh.
func TestPeersFromToleratesAnUnusableRow(t *testing.T) {
	good, _ := NewKeys()

	peers := PeersFrom([]state.Host{
		{ID: "host-broken", WGPubKey: "not-a-key", PublicIP: "203.0.113.1"},
		{ID: "host-good", WGPubKey: good.Public.String(), PublicIP: "not-an-ip"},
	}, "host-self")

	if len(peers) != 1 {
		t.Fatalf("got %d peers, want the one with a usable key", len(peers))
	}
	if peers[0].PublicKey != good.Public {
		t.Error("the wrong host survived")
	}
	// An unparseable public address is not fatal: the peer can still reach us,
	// and the tunnel comes up from its side.
	if peers[0].Endpoint.IsValid() {
		t.Error("an unusable public address became an endpoint")
	}
}

func TestPeersFromAcceptsAHostWithNoPublicAddressYet(t *testing.T) {
	k, _ := NewKeys()
	peers := PeersFrom([]state.Host{{ID: "host-b", WGPubKey: k.Public.String()}}, "host-a")

	if len(peers) != 1 {
		t.Fatalf("got %d peers", len(peers))
	}
	if peers[0].Endpoint.IsValid() {
		t.Error("an endpoint was invented for a host with no public address")
	}
	if peers[0].Address != AddressFor(k.Public) {
		t.Error("the peer is still addressable on the mesh")
	}
	var zero wgtypes.Key
	if peers[0].PublicKey == zero {
		t.Error("peer has no key")
	}
}
