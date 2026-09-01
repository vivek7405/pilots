package mesh

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	// LinkName is the mesh interface. Fleet-wide, so anything reading `wg show`
	// on any host sees the same name.
	LinkName = "pilots0"

	// ListenPort is the UDP port WireGuard itself uses, on the PUBLIC
	// interface. Gossip and the internal proxy ride inside the tunnel; this is
	// the only mesh port that has to be reachable from outside.
	ListenPort = 51820

	// MTU is conservative on purpose: the standard 1500-byte underlay less
	// WireGuard's worst-case encapsulation (outer IPv6 40 + UDP 8 + header and
	// auth tag 32). Guessing high black-holes large packets in a way that
	// looks like random loss rather than an MTU problem.
	MTU = 1500 - 80

	// keepalive keeps NAT and stateful firewalls from forgetting the tunnel.
	// Hosts are behind neither in the target deployment, but a peer that goes
	// quiet for minutes is indistinguishable from one that died.
	keepalive = 25 * time.Second
)

// Peer is another host on the mesh.
type Peer struct {
	PublicKey wgtypes.Key
	// Endpoint is where this host's packets to the peer are sent: its public
	// address and the WireGuard port.
	Endpoint netip.AddrPort
	// Address is the peer's mesh address, derived from its key.
	Address netip.Addr
}

// Device is this host's WireGuard interface.
type Device struct {
	keys Keys
	ctrl *wgctrl.Client
}

// Open brings the interface up and configures it for these keys.
//
// Idempotent from end to end: the link is created only if absent, the address
// added only if missing, the device reconfigured in place. hostd restarts
// without disturbing a mesh that is already carrying gossip and proxied
// requests.
func Open(keys Keys) (*Device, error) {
	link, err := ensureLink()
	if err != nil {
		return nil, err
	}

	ctrl, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("mesh: open wireguard control: %w", err)
	}

	d := &Device{keys: keys, ctrl: ctrl}

	port := ListenPort
	if err := ctrl.ConfigureDevice(LinkName, wgtypes.Config{
		PrivateKey: &keys.Private,
		ListenPort: &port,
	}); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("mesh: configure %s: %w", LinkName, err)
	}

	if err := ensureAddress(link, keys.Address()); err != nil {
		ctrl.Close()
		return nil, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("mesh: bring %s up: %w", LinkName, err)
	}
	return d, nil
}

// Address is this host's mesh address.
func (d *Device) Address() netip.Addr { return d.keys.Address() }

// PublicKey is this host's mesh identity.
func (d *Device) PublicKey() wgtypes.Key { return d.keys.Public }

func (d *Device) Close() error { return d.ctrl.Close() }

// Sync makes the device's peers match want, exactly.
//
// A diff against what the device currently has, not a rewrite: replacing every
// peer on each pass tears down working tunnels and drops the gossip and
// proxied requests riding them, every time any host's row changes. Since the
// peer set is recomputed from the hosts cache on every change, that would be
// often.
func (d *Device) Sync(want []Peer) error {
	current, err := d.ctrl.Device(LinkName)
	if err != nil {
		return fmt.Errorf("mesh: read %s: %w", LinkName, err)
	}

	have := make(map[wgtypes.Key]wgtypes.Peer, len(current.Peers))
	for _, p := range current.Peers {
		have[p.PublicKey] = p
	}
	wanted := make(map[wgtypes.Key]Peer, len(want))

	var configs []wgtypes.PeerConfig
	for _, p := range want {
		if p.PublicKey == d.keys.Public {
			continue // never peer with ourselves
		}
		wanted[p.PublicKey] = p

		existing, known := have[p.PublicKey]
		if known && peerMatches(existing, p) {
			continue
		}
		configs = append(configs, peerConfig(p))
	}

	// Anything on the device that is no longer wanted goes. A host that left
	// the fleet must stop being a route, or traffic for a machine it no longer
	// owns keeps being sent to it.
	for key := range have {
		if _, ok := wanted[key]; !ok {
			configs = append(configs, wgtypes.PeerConfig{PublicKey: key, Remove: true})
		}
	}

	if len(configs) == 0 {
		return nil
	}
	if err := d.ctrl.ConfigureDevice(LinkName, wgtypes.Config{Peers: configs}); err != nil {
		return fmt.Errorf("mesh: reconcile peers: %w", err)
	}
	slog.Info("mesh peers reconciled", "changed", len(configs), "total", len(wanted))

	return d.syncRoutes(want)
}

// peerConfig builds the wgtypes form of a peer.
func peerConfig(p Peer) wgtypes.PeerConfig {
	interval := keepalive
	cfg := wgtypes.PeerConfig{
		PublicKey: p.PublicKey,
		// The peer's mesh address and nothing else. AllowedIPs is WireGuard's
		// routing table AND its inbound filter, so a wider range would let one
		// host claim traffic for addresses that are not its own.
		AllowedIPs:                  []net.IPNet{hostRoute(p.Address)},
		ReplaceAllowedIPs:           true,
		PersistentKeepaliveInterval: &interval,
	}
	if p.Endpoint.IsValid() {
		cfg.Endpoint = &net.UDPAddr{
			IP:   p.Endpoint.Addr().AsSlice(),
			Port: int(p.Endpoint.Port()),
		}
	}
	return cfg
}

// peerMatches reports whether the device already has this peer configured.
func peerMatches(have wgtypes.Peer, want Peer) bool {
	if want.Endpoint.IsValid() {
		if have.Endpoint == nil {
			return false
		}
		if !have.Endpoint.IP.Equal(net.IP(want.Endpoint.Addr().AsSlice())) ||
			have.Endpoint.Port != int(want.Endpoint.Port()) {
			return false
		}
	}
	route := hostRoute(want.Address)
	if len(have.AllowedIPs) != 1 {
		return false
	}
	return have.AllowedIPs[0].IP.Equal(route.IP) &&
		have.AllowedIPs[0].Mask.String() == route.Mask.String()
}

// syncRoutes adds a route to each peer through the mesh interface.
//
// Separate from the WireGuard config because they are different subsystems:
// AllowedIPs tells WireGuard which peer a packet belongs to, while the kernel
// still needs a route to decide the packet goes to the mesh at all.
func (d *Device) syncRoutes(peers []Peer) error {
	link, err := netlink.LinkByName(LinkName)
	if err != nil {
		return fmt.Errorf("mesh: find %s: %w", LinkName, err)
	}

	var errs []error
	for _, p := range peers {
		if p.PublicKey == d.keys.Public {
			continue
		}
		route := hostRoute(p.Address)
		if err := netlink.RouteReplace(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &route,
			Scope:     netlink.SCOPE_LINK,
		}); err != nil {
			errs = append(errs, fmt.Errorf("mesh: route to %s: %w", p.Address, err))
		}
	}
	return errors.Join(errs...)
}

// hostRoute is the single-address prefix for a mesh address.
func hostRoute(addr netip.Addr) net.IPNet {
	return net.IPNet{IP: addr.AsSlice(), Mask: net.CIDRMask(128, 128)}
}

// ensureLink creates the interface if it does not exist.
func ensureLink() (netlink.Link, error) {
	link, err := netlink.LinkByName(LinkName)
	if err == nil {
		return link, nil
	}
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		return nil, fmt.Errorf("mesh: look up %s: %w", LinkName, err)
	}

	attrs := netlink.NewLinkAttrs()
	attrs.Name = LinkName
	attrs.MTU = MTU
	if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: attrs}); err != nil {
		return nil, fmt.Errorf("mesh: create %s: %w", LinkName, err)
	}
	link, err = netlink.LinkByName(LinkName)
	if err != nil {
		return nil, fmt.Errorf("mesh: find %s after creating it: %w", LinkName, err)
	}
	return link, nil
}

// ensureAddress puts this host's mesh address on the interface.
func ensureAddress(link netlink.Link, addr netip.Addr) error {
	want := &netlink.Addr{IPNet: &net.IPNet{
		IP: addr.AsSlice(), Mask: net.CIDRMask(128, 128),
	}}

	existing, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return fmt.Errorf("mesh: list addresses on %s: %w", LinkName, err)
	}
	for _, a := range existing {
		if a.IP.Equal(want.IP) {
			return nil
		}
		// A stale address from a previous identity would keep answering for a
		// host that no longer exists.
		if err := netlink.AddrDel(link, &a); err != nil {
			slog.Warn("could not remove a stale mesh address", "addr", a.IP, "err", err)
		}
	}

	if err := netlink.AddrAdd(link, want); err != nil {
		return fmt.Errorf("mesh: add %s to %s: %w", addr, LinkName, err)
	}
	return nil
}
