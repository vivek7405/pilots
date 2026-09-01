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

// A joining host's peers come from the hosts table, the table arrives by
// gossip, and gossip rides this mesh. With an empty table there is no route to
// anywhere, so the table stays empty forever. The bootstrap peer is the one
// edge that is configured rather than discovered, and it is what breaks that
// circle -- so it must survive a reconcile against an empty table.
func TestTheBootstrapPeerSurvivesAnEmptyHostsTable(t *testing.T) {
	peerKeys, _ := NewKeys()
	bootstrap := Peer{
		PublicKey: peerKeys.Public,
		Address:   peerKeys.Address(),
		Endpoint:  netip.MustParseAddrPort("203.0.113.1:51820"),
	}

	got := withBootstrap(PeersFrom(nil, "host-self"), []Peer{bootstrap})
	if len(got) != 1 || got[0].PublicKey != peerKeys.Public {
		t.Fatalf("the bootstrap peer was dropped with an empty hosts table: %+v", got)
	}
}

// Once the peer's own row arrives, the row is the description used -- but the
// peer must not appear twice, which would make Sync reconfigure it every pass.
func TestTheBootstrapPeerIsNotDuplicatedOnceItsRowArrives(t *testing.T) {
	peerKeys, _ := NewKeys()
	bootstrap := Peer{
		PublicKey: peerKeys.Public,
		Address:   peerKeys.Address(),
		Endpoint:  netip.MustParseAddrPort("203.0.113.1:51820"),
	}

	fromTable := PeersFrom([]state.Host{{
		ID: "host-peer", WGPubKey: peerKeys.Public.String(), PublicIP: "203.0.113.1",
	}}, "host-self")

	got := withBootstrap(fromTable, []Peer{bootstrap})
	if len(got) != 1 {
		t.Errorf("peer appears %d times once its row arrived", len(got))
	}
}

func TestParseBootstrapPeer(t *testing.T) {
	keys, _ := NewKeys()
	spec := keys.Public.String() + "@203.0.113.7:51820"

	peer, err := ParseBootstrapPeer(spec)
	if err != nil {
		t.Fatalf("ParseBootstrapPeer(%q): %v", spec, err)
	}
	if peer.PublicKey != keys.Public {
		t.Error("key did not round-trip")
	}
	// The address is DERIVED from the key, because the key is the only thing
	// that determines it -- and a spec carrying only an address could never
	// produce a tunnel.
	if peer.Address != keys.Address() {
		t.Errorf("address = %s, want the one the key derives to %s", peer.Address, keys.Address())
	}
	if peer.Endpoint != netip.MustParseAddrPort("203.0.113.7:51820") {
		t.Errorf("endpoint = %s", peer.Endpoint)
	}

	for _, bad := range []string{"no-at-sign", "not-a-key@203.0.113.7:51820", keys.Public.String() + "@nonsense"} {
		if _, err := ParseBootstrapPeer(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
