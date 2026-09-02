package main

import (
	"errors"
	"log"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
