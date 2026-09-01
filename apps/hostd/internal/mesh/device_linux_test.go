package mesh

import (
	"net/netip"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
)

func requireMesh(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: creating a WireGuard interface")
	}
}

// openTestDevice brings the real interface up and tears it down afterwards.
func openTestDevice(t *testing.T) *Device {
	t.Helper()
	requireMesh(t)

	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := Open(keys)
	if err != nil {
		if link, lerr := netlink.LinkByName(LinkName); lerr == nil {
			_ = netlink.LinkDel(link)
		}
		t.Skipf("could not open the mesh interface (is the wireguard module loaded?): %v", err)
	}
	t.Cleanup(func() {
		dev.Close()
		if link, err := netlink.LinkByName(LinkName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	return dev
}

// The host has to be reachable at the address its key derives to, or every
// peer's route points somewhere nothing answers.
func TestOpenConfiguresTheInterface(t *testing.T) {
	dev := openTestDevice(t)

	link, err := netlink.LinkByName(LinkName)
	if err != nil {
		t.Fatalf("interface was not created: %v", err)
	}
	if link.Attrs().OperState == netlink.OperDown {
		t.Error("interface is down")
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	want := dev.Address()
	found := false
	for _, a := range addrs {
		if a.IP.Equal(want.AsSlice()) {
			found = true
		}
	}
	if !found {
		t.Errorf("the derived address %s is not on the interface: %v", want, addrs)
	}

	ctrl, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	wgDev, err := ctrl.Device(LinkName)
	if err != nil {
		t.Fatalf("wgctrl could not read the device: %v", err)
	}
	if wgDev.PublicKey != dev.PublicKey() {
		t.Error("the device carries a different key than the host's identity")
	}
	if wgDev.ListenPort != ListenPort {
		t.Errorf("listening on %d, want %d", wgDev.ListenPort, ListenPort)
	}
}

// hostd restarts without disturbing a mesh that is already carrying gossip and
// proxied requests, so opening an interface that already exists must be a
// no-op rather than a failure.
func TestOpenIsIdempotent(t *testing.T) {
	dev := openTestDevice(t)
	addr := dev.Address()

	again, err := Open(Keys{Private: dev.keys.Private, Public: dev.keys.Public})
	if err != nil {
		t.Fatalf("re-opening an existing interface failed: %v", err)
	}
	defer again.Close()

	if again.Address() != addr {
		t.Errorf("address changed on re-open: %s then %s", addr, again.Address())
	}
}

// Peers are reconciled on every hosts-table change. Sync must add what is new,
// leave what is unchanged alone, and remove what has left -- a host that is
// gone must stop being a route, or traffic for machines it no longer owns
// keeps being sent to it.
func TestSyncReconcilesPeers(t *testing.T) {
	dev := openTestDevice(t)

	ctrl, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	peerKeys := make([]Keys, 3)
	peers := make([]Peer, 3)
	for i := range peerKeys {
		k, err := NewKeys()
		if err != nil {
			t.Fatal(err)
		}
		peerKeys[i] = k
		peers[i] = Peer{
			PublicKey: k.Public,
			Address:   k.Address(),
			Endpoint:  netip.MustParseAddrPort("203.0.113." + string(rune('1'+i)) + ":51820"),
		}
	}

	if err := dev.Sync(peers); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := devicePeerCount(t, ctrl); got != 3 {
		t.Fatalf("device has %d peers after adding 3", got)
	}

	// One host leaves.
	if err := dev.Sync(peers[:2]); err != nil {
		t.Fatalf("Sync after a host left: %v", err)
	}
	if got := devicePeerCount(t, ctrl); got != 2 {
		t.Errorf("device has %d peers after one left, want 2", got)
	}

	wgDev, err := ctrl.Device(LinkName)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range wgDev.Peers {
		if p.PublicKey == peerKeys[2].Public {
			t.Error("the departed host is still a peer, so traffic still routes to it")
		}
		if len(p.AllowedIPs) != 1 {
			t.Errorf("peer allows %d prefixes, want its own address only", len(p.AllowedIPs))
		}
	}
}

// A host must never peer with itself: it would install a route for its own
// address through the tunnel, and its own traffic would leave and come back.
func TestSyncSkipsSelf(t *testing.T) {
	dev := openTestDevice(t)

	if err := dev.Sync([]Peer{{
		PublicKey: dev.PublicKey(),
		Address:   dev.Address(),
	}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	ctrl, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	if got := devicePeerCount(t, ctrl); got != 0 {
		t.Errorf("the host peered with itself (%d peers)", got)
	}
}

func devicePeerCount(t *testing.T, ctrl *wgctrl.Client) int {
	t.Helper()
	dev, err := ctrl.Device(LinkName)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}
	return len(dev.Peers)
}
