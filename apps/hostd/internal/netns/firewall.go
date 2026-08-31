package netns

import (
	"fmt"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	vnetns "github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// applyNftables installs the namespace's NAT and egress firewall.
//
// NAT is a whole-address 1:1 translation, not a port mapping: outbound traffic
// from the constant guest address is SNATed to the slot's host-facing address,
// and inbound traffic to the slot address is DNATed back to the guest. That is
// what lets every guest in the fleet share one address while remaining
// individually routable -- and it lives here, in namespace state rebuilt on
// every restore, rather than anywhere a snapshot could capture it.
func applyNftables(ns vnetns.NsHandle, s *Slot) error {
	c, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return fmt.Errorf("netns: nftables conn: %w", err)
	}
	defer c.CloseLasting()

	guestIP := net.ParseIP(TapGuestIP).To4()
	hostIP := s.HostIP.To4()
	if guestIP == nil || hostIP == nil {
		return fmt.Errorf("netns: slot %d has non-IPv4 addressing", s.Idx)
	}

	// IPv4-only NAT table. The inet family's nat support needs a newer kernel
	// and buys nothing here, since the addresses being translated are v4.
	nat := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "pilots-nat"})

	post := c.AddChain(&nftables.Chain{
		Name: "postrouting", Table: nat,
		Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource, Policy: chainPolicy(nftables.ChainPolicyAccept),
	})
	// Outbound: src 169.254.0.21 -> src 10.11.x.y
	c.AddRule(&nftables.Rule{
		Table: nat, Chain: post,
		Exprs: append(matchIPv4(srcOffset, guestIP),
			&expr.Immediate{Register: 1, Data: hostIP},
			&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
		),
	})

	pre := c.AddChain(&nftables.Chain{
		Name: "prerouting", Table: nat,
		Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest, Policy: chainPolicy(nftables.ChainPolicyAccept),
	})
	// Inbound: dst 10.11.x.y -> dst 169.254.0.21
	c.AddRule(&nftables.Rule{
		Table: nat, Chain: pre,
		Exprs: append(matchIPv4(dstOffset, hostIP),
			&expr.Immediate{Register: 1, Data: guestIP},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
		),
	})

	// Egress firewall. Rule ORDER is the whole design here: the slot's own
	// 10.11 address is accepted BEFORE the 10.0.0.0/8 drop, or the machine
	// could not be reached at all. Priority -150 puts this ahead of
	// nat-prerouting (-100), so inbound packets still carry the pre-DNAT
	// destination when these rules match.
	filter := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "pilots-firewall"})
	fchain := c.AddChain(&nftables.Chain{
		Name: "prerouting", Table: filter,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityRef(-150), Policy: chainPolicy(nftables.ChainPolicyAccept),
	})

	// Return traffic first, before any drop can catch it.
	c.AddRule(&nftables.Rule{Table: filter, Chain: fchain, Exprs: []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryU32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:  binaryU32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryU32(0)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})

	// The machine's own link-local /30 and its slot address.
	for _, allow := range []struct {
		ip   net.IP
		bits int
	}{
		{net.ParseIP("169.254.0.20").To4(), 30},
		{hostIP, 32},
	} {
		c.AddRule(&nftables.Rule{Table: filter, Chain: fchain,
			Exprs: append(matchIPv4Net(dstOffset, allow.ip, allow.bits),
				&expr.Verdict{Kind: expr.VerdictAccept})})
	}

	// Everything private is denied: other tenants' slots (10/8), the host's
	// own services (127/8), cloud metadata and the rest of link-local
	// (169.254/16), and whatever LAN the host sits on.
	for _, deny := range []struct {
		cidr string
		bits int
	}{
		{"10.0.0.0", 8},
		{"127.0.0.0", 8},
		{"169.254.0.0", 16},
		{"172.16.0.0", 12},
		{"192.168.0.0", 16},
	} {
		c.AddRule(&nftables.Rule{Table: filter, Chain: fchain,
			Exprs: append(matchIPv4Net(dstOffset, net.ParseIP(deny.cidr).To4(), deny.bits),
				&expr.Verdict{Kind: expr.VerdictDrop})})
	}

	// IPv6 equivalents: loopback, unique-local, link-local.
	for _, deny := range []struct {
		cidr string
		bits int
	}{
		{"::1", 128},
		{"fc00::", 7},
		{"fe80::", 10},
	} {
		c.AddRule(&nftables.Rule{Table: filter, Chain: fchain,
			Exprs: append(matchIPv6Net(dstOffset6, net.ParseIP(deny.cidr).To16(), deny.bits),
				&expr.Verdict{Kind: expr.VerdictDrop})})
	}

	if err := c.Flush(); err != nil {
		return fmt.Errorf("netns: apply nftables for slot %d: %w", s.Idx, err)
	}
	return nil
}

// IPv4 header offsets for source and destination addresses.
const (
	srcOffset  = 12
	dstOffset  = 16
	dstOffset6 = 24 // IPv6 destination
)

func chainPolicy(p nftables.ChainPolicy) *nftables.ChainPolicy { return &p }

func binaryU32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// matchIPv4 matches an exact IPv4 address at the given header offset.
func matchIPv4(offset uint32, ip net.IP) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ip},
	}
}

// matchIPv4Net matches an IPv4 prefix by masking before comparing.
func matchIPv4Net(offset uint32, ip net.IP, bits int) []expr.Any {
	mask := net.CIDRMask(bits, 32)
	network := ip.Mask(mask)
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network},
	}
}

// matchIPv6Net matches an IPv6 prefix.
func matchIPv6Net(offset uint32, ip net.IP, bits int) []expr.Any {
	mask := net.CIDRMask(bits, 128)
	network := ip.Mask(mask)
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 16},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 16, Mask: mask, Xor: make([]byte, 16)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network},
	}
}
