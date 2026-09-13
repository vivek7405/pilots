package main

import (
	"errors"
	"log"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// The guest's IPv6 half of its link, configured by the agent when it is PID 1.
//
// Imported from the packages that OWN these values rather than copied: the
// agent is always compiled inside this module (build-golden-rootfs.sh and
// host-bootstrap.sh both build ./cmd/guest-agent from apps/hostd, and only
// the binary is copied into an image), so the guest can never drift from
// what the host translates. scripts/rootfs/eth0.network carries the same
// values for the golden rootfs; netns's tests pin that copy.
const (
	guestIP6 = netns.TapGuestIP6 + "/126"
	gateway6 = netns.TapHostIP6
)

var peerPrefix = mesh.MachineSpace.String()

// configureNetwork gives eth0 its IPv6 address and the route to its peers.
//
// The golden rootfs gets these from systemd-networkd. An image built from a
// user's Dockerfile does not: the kernel is told to run this binary as init,
// so whatever the image would have started never does, and nothing else
// configures the link. Nothing ever reported that: the kernel's ip=
// boot argument sets up IPv4, the machine boots, serves, and answers health
// checks, and only .internal is quietly missing. DNS still resolves a peer's
// name because that is answered on the host, and the tenant filter still
// permits the traffic; the guest simply has no address to send from and no
// route to send on.
//
// So a service built from a Dockerfile could not reach another service by
// name, which is the whole point of putting two of them in one app.
//
// Best effort by design. A machine with no v6 is a machine that cannot use
// .internal, which is worse than not booting only if you believe .internal is
// optional -- but refusing to boot over it would take out every single-service
// app as well, and those work fine.
func configureNetwork() {
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		log.Printf("guest-agent: no eth0, so no .internal: %v", err)
		return
	}

	addr, err := netlink.ParseAddr(guestIP6)
	if err != nil {
		log.Printf("guest-agent: bad guest address %q: %v", guestIP6, err)
		return
	}
	// EEXIST is success. This runs on every agent start, including a restart
	// of the systemd unit and an image whose own init already configured the
	// link, and in those cases the address is simply already there.
	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, unix.EEXIST) {
		log.Printf("guest-agent: could not add %s: %v", guestIP6, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		log.Printf("guest-agent: could not bring eth0 up: %v", err)
		return
	}

	globalScopeV4(link)

	_, dst, err := net.ParseCIDR(peerPrefix)
	if err != nil {
		log.Printf("guest-agent: bad peer prefix %q: %v", peerPrefix, err)
		return
	}
	// Checked rather than passed straight in. netlink treats a nil Gw as "no
	// gateway", so a typo in the constant would install an on-link route that
	// looks right in `ip -6 route` and silently reaches nobody.
	gw := net.ParseIP(gateway6)
	if gw == nil {
		log.Printf("guest-agent: bad gateway address %q", gateway6)
		return
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Gw:        gw,
	}); err != nil && !errors.Is(err, unix.EEXIST) {
		log.Printf("guest-agent: could not route %s via %s, so peers are "+
			"unreachable by name: %v", peerPrefix, gateway6, err)
	}
}

// globalScopeV4 puts eth0's IPv4 address into global scope.
//
// # The bug
//
// A guest could not reach the internet. Not slowly, not intermittently: every
// outbound connection to a public address failed, on every machine, and had
// since the addressing was chosen.
//
// The kernel's `ip=` boot argument gives eth0 169.254.0.21, and systemd-networkd
// configures the same address in the golden rootfs. Both assign it LINK scope,
// because 169.254.0.0/16 is link-local and that is what the address means. A
// link-scoped address cannot be chosen as the source for a route to a GLOBAL
// destination, so the guest had nothing to source from and built its packets
// with source 0.0.0.0:
//
//	IP 0.0.0.0.50696 > 1.1.1.1.443: Flags [S]
//
// which the kernel drops as a martian before it leaves the namespace. Nothing
// logged it anywhere. The packet capture on the tap is the only place it was
// ever visible, and it took a masquerade fix and a forwarding fix -- both of
// them genuinely necessary, neither of them sufficient -- before there was
// anything downstream left to blame.
//
// # Why the address stays link-local
//
// Because that is the whole point of it: every guest in the fleet has the same
// 169.254.0.21, and the host translates it per machine in namespace state that
// is rebuilt on every restore. An address that were globally unique per machine
// would go into the snapshot and break invariant 5. The scope is a statement
// about how the KERNEL may use the address, and this one is used as a source
// for traffic the namespace then translates -- so global is the truthful scope
// here even though the range is not.
//
// Best effort, like the rest of this function: a machine that cannot reach the
// internet is worse than one that can, and both are better than one that does
// not boot.
func globalScopeV4(link netlink.Link) {
	addrs, err := netlink.AddrList(link, unix.AF_INET)
	if err != nil {
		log.Printf("guest-agent: could not list eth0's IPv4 addresses, so its "+
			"scope is unchecked and outbound traffic may have no source: %v", err)
		return
	}
	for _, addr := range addrs {
		if addr.IP == nil || addr.IP.To4() == nil {
			continue
		}
		if addr.Scope == int(unix.RT_SCOPE_UNIVERSE) {
			continue // already global, which is every boot after the first
		}
		fixed := addr
		fixed.Scope = int(unix.RT_SCOPE_UNIVERSE)
		// Replace rather than delete-then-add: a window with no address at all
		// is a window where the agent's own listener has nothing to bind, and
		// this runs while the machine is coming up.
		if err := netlink.AddrReplace(link, &fixed); err != nil {
			log.Printf("guest-agent: could not put %s into global scope, so "+
				"outbound connections will have no source address: %v", addr.IP, err)
			continue
		}
		log.Printf("guest-agent: %s is now globally scoped, so the guest can "+
			"source outbound traffic", addr.IP)
	}
}
