package main

import (
	"log"
	"net"

	"github.com/vishvananda/netlink"
)

// The guest's IPv6 half of its link, configured by the agent when it is PID 1.
//
// These are the same constants scripts/rootfs/eth0.network declares, and they
// have to stay identical: every guest in the fleet shares them, which is what
// makes a snapshot host-agnostic.
const (
	guestIP6   = "fdee::21/126"
	gateway6   = "fdee::22"
	peerPrefix = "fdcd::/16"
)

// configureNetwork gives eth0 its IPv6 address and the route to its peers.
//
// The golden rootfs gets these from systemd-networkd. An image built from a
// user's Dockerfile has no init at all -- this binary IS its init -- so
// nothing configures them, and nothing ever reported that: the kernel's ip=
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
	if err := netlink.AddrAdd(link, addr); err != nil {
		// Already present is not a failure: the image may carry an init that
		// configured it, in which case this is a no-op and the route below
		// still needs attempting.
		log.Printf("guest-agent: could not add %s (may already be set): %v", guestIP6, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		log.Printf("guest-agent: could not bring eth0 up: %v", err)
		return
	}

	_, dst, err := net.ParseCIDR(peerPrefix)
	if err != nil {
		return
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Gw:        net.ParseIP(gateway6),
	}); err != nil {
		log.Printf("guest-agent: could not route %s via %s, so peers are "+
			"unreachable by name: %v", peerPrefix, gateway6, err)
	}
}
