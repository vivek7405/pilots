package netns

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// egressTable is the root-namespace table this file owns, end to end. Named so
// `nft list table inet pilots-egress` on a host shows exactly what is applied
// and nothing else: no rule here is added to a table anything else writes.
const egressTable = "pilots-egress"

// EgressConfig is what an operator has to say to turn PER-ORG egress on.
//
// Both fields empty is the default and means per-org addresses are OFF: no
// source rewrite, no address assigned, and every guest leaves behind the
// host's shared address. That part is deliberate. Guest outbound traffic is
// the one path where a wrong rule is invisible until a tenant's application
// stops being able to reach the internet, so no traffic is rewritten until a
// host is told to rewrite it.
//
// What is NOT optional is the masquerade. A guest's packets reach the root
// namespace wearing the slot's 10.11 address, which is routable nowhere, so
// without a masquerade a guest has no outbound IPv4 at all -- and this config
// being empty is the default, which meant the default fleet had none. The
// battery caught it on the rig: the host could reach 1.1.1.1 and the guest
// could not. So the table is installed either way, and this config decides
// only what goes in it beyond the masquerade.
type EgressConfig struct {
	// Interface is the uplink guest traffic leaves by. Masquerade is scoped to
	// it rather than applied unconditionally, so traffic between hosts on a
	// private network is not rewritten on its way out.
	Interface string
	// Prefix6 is a globally routed IPv6 block delegated to this host. Each org
	// gets one address out of it.
	Prefix6 netip.Prefix
}

// Enabled reports whether this host has been told to manage egress at all.
func (c EgressConfig) Enabled() bool { return c.Interface != "" }

// Validate is what the config must satisfy before hostd starts.
//
// A half-configured host is refused rather than started: an interface with no
// prefix would masquerade every guest behind the shared address and report
// per-org addresses to nobody, which is the old behaviour wearing the new
// feature's name.
func (c EgressConfig) Validate() error {
	if !c.Enabled() {
		if c.Prefix6.IsValid() {
			return fmt.Errorf("netns: an egress prefix was given with no egress interface; " +
				"pass both, or neither")
		}
		return nil
	}
	if !c.Prefix6.IsValid() {
		return fmt.Errorf("netns: an egress interface was given with no egress prefix; " +
			"without one there is no per-org address to hand out")
	}
	return ValidEgressPrefix(c.Prefix6)
}

// Fingerprint is a stable summary of what is applied, so the loop driving this
// rebuilds the table only when the fleet actually moves.
//
// The same reasoning as TenantRules.Fingerprint: the loop ticks far more often
// than machines come and go, and a rebuild is a netlink transaction
// proportional to the number of machines on the host.
func (p EgressPlan) Fingerprint(c EgressConfig, uplink string) string {
	h := sha256.New()
	// The resolved uplink, not just the configured one. On a host that named
	// no interface it comes from the default route, and a default route that
	// moves has to rebuild the table -- otherwise the masquerade stays scoped
	// to an interface the traffic no longer leaves by, and every guest loses
	// the internet with nothing in the log.
	fmt.Fprintf(h, "C|%s|%s|%s\n", c.Interface, c.Prefix6, uplink)
	for _, a := range p.Addrs {
		fmt.Fprintf(h, "A|%s\n", a)
	}
	for _, r := range p.Rules {
		fmt.Fprintf(h, "R|%s|%s\n", r.Machine6, r.To)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ApplyEgress installs the root-namespace egress table and puts each org's
// address on the uplink.
//
// A disabled config still installs the table, holding the masquerade alone.
// Turning per-org addresses off must stop the REWRITING -- an operator who
// thinks they have reverted and whose traffic still leaves from addresses
// nothing documents is the worst of both -- but it must not take the guests
// off the internet, which is what removing the whole table did.
//
// uplink is the interface guest traffic leaves by, already resolved: the
// configured one, or the default route's when none was configured.
func ApplyEgress(c EgressConfig, plan EgressPlan, uplink string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if uplink == "" {
		return fmt.Errorf("netns: no uplink interface to masquerade guest traffic out of; " +
			"set PILOT_EGRESS_INTERFACE, or give this host a default route")
	}

	// IPv4 forwarding in the ROOT namespace, without which the masquerade
	// below is decoration.
	//
	// setup.go turns forwarding on inside each machine's own namespace, which
	// gets a packet from the guest's tap to the veth. Getting it from the veth
	// to the uplink is the root namespace's job, and nothing turned it on
	// there -- so with the masquerade rule installed, correct, and scoped to
	// the right interface, a guest still could not reach the internet and
	// nothing anywhere said why.
	//
	// Here rather than in the bootstrap script's sysctl file for the reason
	// ApplyTenantFilter writes the v6 knob here: the rule and the knob are
	// useless apart, so whoever adds one has already added the other.
	if err := os.WriteFile(v4ForwardingKnob, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("netns: enable ipv4 forwarding: %w", err)
	}

	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("netns: nftables conn: %w", err)
	}
	defer conn.CloseLasting()

	// The previous generation goes in the SAME batch. Left in place, its rules
	// keep matching alongside the new ones and a destroyed machine's address
	// keeps leaving as its old org's -- which, once the slot is reused, is
	// another tenant's traffic behind that allowlist entry.
	existing, err := conn.ListTables()
	if err != nil {
		return fmt.Errorf("netns: list tables: %w", err)
	}
	for _, t := range existing {
		if t.Name == egressTable && t.Family == nftables.TableFamilyINet {
			conn.DelTable(t)
		}
	}
	table := conn.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: egressTable})
	post := conn.AddChain(&nftables.Chain{
		Name: "postrouting", Table: table,
		Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   chainPolicy(nftables.ChainPolicyAccept),
	})
	conn.FlushChain(post)

	// Per-org source rewrites FIRST, so a machine that has one is not caught
	// by the masquerade below it. Order is the whole design: masquerade
	// rewrites to whatever address the output interface carries, which is the
	// host's shared one, and a masquerade that ran first would make every rule
	// under it dead code that still reads as if it worked.
	for _, r := range plan.Rules {
		// Empty on a host with no per-org config, which is what makes the
		// masquerade below the only rule in the table there.
		if !c.Enabled() || !r.Machine6.Is6() || !r.To.Is6() {
			continue
		}
		src := r.Machine6.As16()
		to := r.To.As16()
		conn.AddRule(&nftables.Rule{
			Table: table, Chain: post,
			Exprs: concat(
				matchIface(expr.MetaKeyOIFNAME, uplink),
				matchIPv6(srcOffset6, net.IP(src[:])),
				[]expr.Any{
					&expr.Immediate{Register: 1, Data: to[:]},
					&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV6, RegAddrMin: 1},
				},
			),
		})
	}

	// The IPv4 half, and the only thing that gives a guest outbound IPv4 at
	// all: its packets reach the root namespace wearing the slot's 10.11
	// address, which is not routable off this host. There is no per-org IPv4
	// here and there will not be one -- a v4 address is a purchased, scarce
	// resource and a bare-metal host has one.
	conn.AddRule(&nftables.Rule{
		Table: table, Chain: post,
		Exprs: concat(
			matchIface(expr.MetaKeyOIFNAME, uplink),
			matchIPv4Net(srcOffset, hostNetworkIP(), hostNetworkBits()),
			[]expr.Any{&expr.Masq{}},
		),
	})

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("netns: apply the egress table: %w", err)
	}
	if !c.Enabled() {
		// No per-org addresses to put on the uplink, and nothing of ours up
		// there to take down: assignEgressAddrs decides what is ours by the
		// configured prefix, and there is none.
		return nil
	}
	return assignEgressAddrs(c, plan.Addrs)
}

// UplinkInterface is the interface guest traffic leaves this host by.
//
// The configured one when an operator named it, because they know which of a
// bare-metal host's interfaces is the uplink and a guess could rewrite traffic
// on a private network between hosts. Otherwise the default route's, which is
// the same answer on every host that has one and is what makes a masquerade
// possible on a fleet that configured no egress at all.
//
// An error rather than a guess when there is no default route: masquerading
// out of the wrong interface is worse than not masquerading, and a host with
// no default route has no internet for a guest to reach anyway.
func UplinkInterface(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return "", fmt.Errorf("netns: list routes to find the uplink: %w", err)
	}
	for _, r := range routes {
		// A default route is one with no destination prefix. Table and metric
		// are left alone: a host with several is an operator who should name
		// the interface, and picking among them here would be the guess this
		// function exists to avoid.
		if r.Dst != nil && !r.Dst.IP.IsUnspecified() {
			continue
		}
		if r.LinkIndex <= 0 {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil {
			continue
		}
		return link.Attrs().Name, nil
	}
	return "", fmt.Errorf("netns: this host has no default IPv4 route, so there is no " +
		"interface to masquerade guest traffic out of")
}

// assignEgressAddrs puts each org's /128 on the uplink and takes down the ones
// no org needs any more.
//
// Without the address on the interface the SNAT still rewrites, and every
// reply is then routed to an address this host never claimed: the connection
// simply never completes, which reads like the remote end refusing rather than
// like a missing line of local configuration.
func assignEgressAddrs(c EgressConfig, want []netip.Addr) error {
	link, err := netlink.LinkByName(c.Interface)
	if err != nil {
		return fmt.Errorf("netns: egress interface %q: %w", c.Interface, err)
	}
	have, err := netlink.AddrList(link, unix.AF_INET6)
	if err != nil {
		return fmt.Errorf("netns: list addresses on %s: %w", c.Interface, err)
	}

	wanted := map[netip.Addr]bool{}
	for _, a := range want {
		wanted[a] = true
	}

	// Remove ours that are no longer wanted, and ONLY ours: an address is
	// ours when it is a /128 inside the configured prefix. The host's own
	// address in that prefix is not a /128 as a rule, and anything outside the
	// prefix belongs to the operator. Getting this wrong takes a host off the
	// network, so the test is narrow on purpose.
	for _, a := range have {
		addr, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		ones, bits := a.Mask.Size()
		if bits != 128 || ones != 128 || !c.Prefix6.Contains(addr) || wanted[addr] {
			continue
		}
		if err := netlink.AddrDel(link, &a); err != nil {
			return fmt.Errorf("netns: remove egress address %s: %w", addr, err)
		}
	}

	for _, a := range want {
		addr := &netlink.Addr{IPNet: &net.IPNet{
			IP: net.IP(a.AsSlice()), Mask: net.CIDRMask(128, 128),
		}}
		if err := netlink.AddrAdd(link, addr); err != nil && !isExists(err) {
			return fmt.Errorf("netns: add egress address %s: %w", a, err)
		}
	}
	return nil
}

// isExists reports an "already there" from netlink, which is the ordinary
// answer on every tick after the first and not a failure.
func isExists(err error) bool {
	return err != nil && (err == unix.EEXIST || err.Error() == "file exists")
}

// hostNetworkIP and hostNetworkBits are the slot range guest traffic reaches
// the root namespace wearing, read off the constant the rest of the package
// uses so the two cannot drift.
func hostNetworkIP() net.IP { n := hostNetwork(); return n.IP }

func hostNetworkBits() int { n := hostNetwork(); ones, _ := n.Mask.Size(); return ones }

func hostNetwork() *net.IPNet {
	_, n, err := net.ParseCIDR(HostNetworkCIDR)
	if err != nil {
		// HostNetworkCIDR is a constant in this package; a parse failure means
		// somebody edited it into something that is not a network.
		panic("netns: HostNetworkCIDR is not a network: " + err.Error())
	}
	return n
}
