package netns

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/vishvananda/netlink"
	vnetns "github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Setup builds the namespace for a slot: veth pair, tap, addresses, routes,
// forwarding, NAT, and the egress firewall.
//
// tapOwnerUID must be the uid the jailer drops Firecracker to, or FC cannot
// open the tap. macAddr is the guest's virtio-net MAC.
//
// Create is teardown-first and therefore idempotent: a create that failed
// partway leaves resources behind, and the next attempt must not trip over
// them.
func Setup(s *Slot, macAddr string, tapOwnerUID int) (err error) {
	_ = Teardown(s) // best effort; a stale namespace must not block a create

	mac, err := net.ParseMAC(macAddr)
	if err != nil {
		return fmt.Errorf("netns: bad mac %q: %w", macAddr, err)
	}

	// Undo everything if any step fails, so a partial namespace never
	// survives to confuse the next create or the reconciler.
	defer func() {
		if err != nil {
			_ = Teardown(s)
		}
	}()

	nsHandle, err := newNamedNS(s.NetnsName)
	if err != nil {
		return err
	}
	defer nsHandle.Close()

	nlh, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return fmt.Errorf("netns: handle for %s: %w", s.NetnsName, err)
	}
	defer nlh.Close()

	// veth pair, with the peer created directly inside the namespace -- no
	// separate move step, so there is no window where the peer is visible on
	// the host.
	veth := &netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: s.VEthName},
		PeerName:      s.VPeerName,
		PeerNamespace: netlink.NsFd(int(nsHandle)),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("netns: add veth %s: %w", s.VEthName, err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		return fmt.Errorf("netns: up %s: %w", s.VEthName, err)
	}
	if err := addAddrHost(veth, s.VEthCIDR()); err != nil {
		return fmt.Errorf("netns: addr on %s: %w", s.VEthName, err)
	}
	if s.HasMesh() {
		if err := addAddrHost(veth, s.VEth6CIDR()); err != nil {
			return fmt.Errorf("netns: v6 addr on %s: %w", s.VEthName, err)
		}
	}

	// Inside the namespace: lo, eth0, and the tap.
	if lo, err := nlh.LinkByName("lo"); err == nil {
		if err := nlh.LinkSetUp(lo); err != nil {
			return fmt.Errorf("netns: up lo: %w", err)
		}
	}

	peer, err := nlh.LinkByName(s.VPeerName)
	if err != nil {
		return fmt.Errorf("netns: find %s in ns: %w", s.VPeerName, err)
	}
	if err := nlh.LinkSetUp(peer); err != nil {
		return fmt.Errorf("netns: up %s: %w", s.VPeerName, err)
	}
	if err := addAddrNS(nlh, peer, s.VPeerCIDR()); err != nil {
		return fmt.Errorf("netns: addr on %s: %w", s.VPeerName, err)
	}
	if s.HasMesh() {
		if err := addAddrNS(nlh, peer, s.VPeer6CIDR()); err != nil {
			return fmt.Errorf("netns: v6 addr on %s: %w", s.VPeerName, err)
		}
	}

	// The tap MUST be created from a thread that is inside the namespace, not
	// through the netlink handle.
	//
	// Creating a tap goes through a TUNSETIFF ioctl on /dev/net/tun, and that
	// device resolves against the CALLING THREAD's network namespace -- it
	// ignores the namespace a netlink handle points at. Using the handle here
	// silently creates the tap on the HOST instead, which then collides with
	// the next machine's tap ("device or resource busy") and leaks an
	// interface into the host namespace.
	//
	// Links, addresses and routes have no such problem, so everything else
	// keeps using the handle and needs no thread switch.
	if err := withNetns(nsHandle, func() error {
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: TapName, HardwareAddr: mac},
			Mode:      netlink.TUNTAP_MODE_TAP,
			// Owner replaces the shell's `ip tuntap add ... user <name>`: the
			// jailer drops Firecracker to this uid, which must be able to
			// open the device.
			Owner: uint32(tapOwnerUID),
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("add tap %s: %w", TapName, err)
		}
		// Re-fetch: the kernel assigns the index on add, and the MAC has to be
		// set on the realised link.
		tapLink, err := netlink.LinkByName(TapName)
		if err != nil {
			return fmt.Errorf("find tap: %w", err)
		}
		if err := netlink.LinkSetHardwareAddr(tapLink, mac); err != nil {
			return fmt.Errorf("set tap mac: %w", err)
		}
		if err := netlink.LinkSetUp(tapLink); err != nil {
			return fmt.Errorf("up tap: %w", err)
		}
		if err := addAddrHost(tapLink, TapHostIP+"/30"); err != nil {
			return fmt.Errorf("addr on tap: %w", err)
		}
		if s.HasMesh() {
			// The v6 gateway the guest routes fdcd::/16 at. Same address in
			// every namespace, which is fine because namespaces do not see
			// each other's links -- and is what keeps it out of the snapshot's
			// way of being host-specific.
			if err := addAddrHost(tapLink, TapHostIP6+"/126"); err != nil {
				return fmt.Errorf("v6 addr on tap: %w", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("netns: tap setup: %w", err)
	}

	// Default route out of the namespace, via the host end of the veth.
	if err := nlh.RouteAdd(&netlink.Route{Gw: s.VEthIP}); err != nil {
		return fmt.Errorf("netns: default route in ns: %w", err)
	}
	if s.HasMesh() {
		// Its v6 counterpart, which is how a translated guest packet leaves
		// for the root namespace and, from there, for the mesh.
		if err := nlh.RouteAdd(&netlink.Route{
			Gw: net.IP(s.VEth6IP.AsSlice()), Family: netlink.FAMILY_V6,
		}); err != nil {
			return fmt.Errorf("netns: default v6 route in ns: %w", err)
		}
	}

	// The matching route on the HOST side. Without it the host has no way to
	// reach the slot address, so replies to the router's own dial are dropped.
	hostRoute := &netlink.Route{
		Dst: &net.IPNet{IP: s.HostIP, Mask: net.CIDRMask(32, 32)},
		Gw:  s.VPeerIP,
	}
	if err := netlink.RouteReplace(hostRoute); err != nil {
		return fmt.Errorf("netns: host route to %s: %w", s.HostIPCIDR(), err)
	}

	// The same thing for the machine's mesh address. This route is the ONLY
	// reason the translation can live inside the namespace: the root namespace
	// needs an unambiguous next hop for a packet addressed to this machine,
	// and the post-translation address (fdee::21) is identical in every one of
	// this host's namespaces, so it can never be that next hop.
	if s.HasMesh() {
		if err := netlink.RouteReplace(machineRoute(s)); err != nil {
			return fmt.Errorf("netns: host route to %s: %w", s.Machine6, err)
		}
	}

	// Forwarding, and then NAT + firewall, all inside the namespace.
	if err := withNetns(nsHandle, func() error {
		knobs := []string{
			"/proc/sys/net/ipv4/ip_forward",
			"/proc/sys/net/ipv4/conf/all/forwarding",
		}
		if s.HasMesh() {
			// Guest-to-guest traffic is forwarded between the tap and the
			// veth, so the namespace forwards v6 as well as v4.
			knobs = append(knobs, "/proc/sys/net/ipv6/conf/all/forwarding")
		}
		for _, knob := range knobs {
			if err := os.WriteFile(knob, []byte("1\n"), 0o644); err != nil {
				return fmt.Errorf("netns: sysctl %s: %w", knob, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return applyNftables(nsHandle, s)
}

// newNamedNS creates /var/run/netns/<name>.
//
// vnetns.NewNamed switches the CALLING THREAD into the new namespace, so the
// thread must be locked and the original namespace restored -- otherwise the
// goroutine can migrate and strand an unrelated thread, or the whole daemon,
// in a machine's namespace.
func newNamedNS(name string) (vnetns.NsHandle, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origin, err := vnetns.Get()
	if err != nil {
		return 0, fmt.Errorf("netns: get current ns: %w", err)
	}
	defer origin.Close()

	handle, err := vnetns.NewNamed(name)
	if err != nil {
		return 0, fmt.Errorf("netns: create %s: %w", name, err)
	}
	if err := vnetns.Set(origin); err != nil {
		handle.Close()
		return 0, fmt.Errorf("netns: restore ns: %w", err)
	}
	return handle, nil
}

// Do runs fn with the calling thread inside a machine's network namespace.
//
// This exists for the same reason the tap is created the way it is: a socket
// belongs to the namespace of the THREAD that created it, and no handle or
// option changes that. hostd binds the .internal responder on the guest's
// gateway address, which only exists inside the namespace, so the bind has to
// happen from in there. Everything afterwards -- accepting, reading, answering
// -- runs wherever the scheduler puts it, because the socket already carries
// the namespace with it.
func Do(netnsName string, fn func() error) error {
	handle, err := vnetns.GetFromName(netnsName)
	if err != nil {
		return fmt.Errorf("netns: open %s: %w", netnsName, err)
	}
	defer handle.Close()
	return withNetns(handle, fn)
}

// withNetns runs fn with the calling thread inside ns, restoring the previous
// namespace afterwards. Used only where an operation reads or writes /proc,
// which is thread-namespace-scoped; link and route work goes through a netlink
// handle instead and needs no thread switch.
func withNetns(ns vnetns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origin, err := vnetns.Get()
	if err != nil {
		return fmt.Errorf("netns: get current ns: %w", err)
	}
	defer origin.Close()

	if err := vnetns.Set(ns); err != nil {
		return fmt.Errorf("netns: enter ns: %w", err)
	}
	// Restore before reporting fn's error: leaving the thread in the wrong
	// namespace is far worse than whatever fn failed at.
	fnErr := fn()
	if err := vnetns.Set(origin); err != nil {
		return fmt.Errorf("netns: restore ns: %w (original error: %v)", err, fnErr)
	}
	return fnErr
}

// machineRoute sends this machine's mesh address into its namespace.
func machineRoute(s *Slot) *netlink.Route {
	return &netlink.Route{
		Dst:    &net.IPNet{IP: s.Machine6.AsSlice(), Mask: net.CIDRMask(128, 128)},
		Gw:     net.IP(s.VPeer6IP.AsSlice()),
		Family: netlink.FAMILY_V6,
	}
}

// addAddrHost assigns an address in the host namespace.
func addAddrHost(link netlink.Link, cidr string) error {
	addr, err := parseAddr(cidr)
	if err != nil {
		return err
	}
	return netlink.AddrAdd(link, addr)
}

// addAddrNS assigns an address through a namespace-scoped handle.
func addAddrNS(nlh *netlink.Handle, link netlink.Link, cidr string) error {
	addr, err := parseAddr(cidr)
	if err != nil {
		return err
	}
	return nlh.AddrAdd(link, addr)
}

// parseAddr reads a CIDR, marking IPv6 addresses as not needing duplicate
// address detection.
//
// DAD would hold every v6 address tentative for about a second after it is
// added, and an address in that state cannot be used -- so a machine created
// and immediately asked to reach a peer would fail for reasons that look like
// packet loss. There is nothing to detect: both ends of a /127 veth and the
// two addresses on the tap are assigned by this function, on links with
// exactly one other node.
func parseAddr(cidr string) (*netlink.Addr, error) {
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return nil, err
	}
	if addr.IP.To4() == nil {
		addr.Flags |= unix.IFA_F_NODAD
	}
	return addr, nil
}
