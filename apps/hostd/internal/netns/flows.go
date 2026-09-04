package netns

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Flow is one established guest-to-guest TCP session as the root namespace
// tracks it: the originator's address and the address it connected to. Both
// are post-translation machine addresses, because translation happens one
// hop earlier (ARCHITECTURE.md, "The data path").
type Flow struct{ Src, Dst netip.Addr }

// OpenFlows lists the established TCP sessions conntrack is carrying.
//
// Conntrack is already there: the tenant chain's established-accept rule is
// what loads it for the forward hook. This reads it rather than adding a
// second observer, and it reads through the netlink module hostd already
// depends on for routes and links.
func OpenFlows() ([]Flow, error) {
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET6)
	if err != nil {
		return nil, fmt.Errorf("netns: list conntrack: %w", err)
	}
	out := make([]Flow, 0, len(flows))
	for _, f := range flows {
		if f.Forward.Protocol != unix.IPPROTO_TCP {
			continue
		}
		tcp, ok := f.ProtoInfo.(*netlink.ProtoInfoTCP)
		if !ok || tcp.State != nl.TCP_CONNTRACK_ESTABLISHED {
			continue
		}
		src, ok1 := netip.AddrFromSlice(f.Forward.SrcIP)
		dst, ok2 := netip.AddrFromSlice(f.Forward.DstIP)
		if !ok1 || !ok2 {
			continue
		}
		out = append(out, Flow{Src: src.Unmap(), Dst: dst.Unmap()})
	}
	return out, nil
}

// HeldBy counts, per machine id, the open sessions to it whose originator is
// a machine that is running right now.
//
// The originator check is the whole point. A client suspended with a pool
// open leaves its flows ESTABLISHED for conntrack's five-day timeout; without
// the check its database could never sleep after it did.
func HeldBy(flows []Flow, running map[netip.Addr]string) map[string]int {
	held := map[string]int{}
	for _, f := range flows {
		if _, ok := running[f.Src]; !ok {
			continue
		}
		if id, ok := running[f.Dst]; ok {
			held[id]++
		}
	}
	return held
}

// ForgetFlowsTo drops conntrack's memory of sessions to addresses nothing is
// behind any more. Called with every suspended replica's address when the
// filter is rebuilt, so a client's stale socket cannot hold a replica up
// after it wakes.
func ForgetFlowsTo(addrs []netip.Addr) error {
	filters := make([]netlink.CustomConntrackFilter, 0, len(addrs))
	for _, a := range addrs {
		if !a.IsValid() {
			continue
		}
		f := &netlink.ConntrackFilter{}
		if err := f.AddIP(netlink.ConntrackOrigDstIP, net.IP(a.AsSlice())); err != nil {
			return err
		}
		filters = append(filters, f)
	}
	if len(filters) == 0 {
		return nil
	}
	_, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, unix.AF_INET6, filters...)
	return err
}
