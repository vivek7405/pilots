package netns

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"sort"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// The tenant boundary for guest-to-guest traffic lives in the ROOT namespace,
// one rule set per host, rebuilt when the fleet changes.
//
// It cannot live inside the namespaces. Doing it there would mean a set of
// every peer address in the app, in up to 1024 namespaces per host,
// re-reconciled on every fleet change -- and churning hardest during a rescue,
// which is exactly when the host is busiest doing everything else.
//
// It also cannot be keyed on source address, which is the shape a reviewer
// expects. Every guest in the fleet sources from the same 169.254.0.21 and the
// same fdee::21, by construction, because that is what makes a snapshot
// restorable on any host. The only thing that identifies the sender is the
// veth the packet arrived on, which is the host's own knowledge and not
// something a compromised guest can put in a header.
//
// The chain's policy is ACCEPT and every rule is an explicit verdict. A drop
// policy on a root-namespace forward chain would take out everything else the
// host forwards, starting with every guest's IPv4 egress.

const (
	// tenantTable is the root-namespace table this file owns entirely. It is
	// deleted and rebuilt on every change, so nothing else may live in it.
	tenantTable = "pilots-tenant"

	// v6ForwardingKnob must be on for a packet to cross from a machine's veth
	// to the mesh at all.
	//
	// Enabling it has a side effect worth knowing before it bites: the kernel
	// stops honouring router advertisements on interfaces whose accept_ra is
	// 1, because a forwarding box is a router. A host that learned its IPv6
	// default route from an RA loses it. scripts/host-bootstrap.sh sets
	// accept_ra=2 for that reason.
	v6ForwardingKnob = "/proc/sys/net/ipv6/conf/all/forwarding"
)

// TenantMachine is one machine as the filter sees it.
type TenantMachine struct {
	// SlotIdx identifies the veth it is reached through. Only set for
	// machines running on THIS host.
	SlotIdx int
	Addr    netip.Addr
	App     string
}

// TenantRules is the desired state of the root-namespace filter.
type TenantRules struct {
	// Local is every machine running on this host, in slot order.
	Local []TenantMachine
	// Apps maps an app to every machine address in it, fleet-wide. Both hosts
	// in a conversation consult their own copy, so the two sides agree only
	// because they read the same replicated rows.
	Apps map[string][]netip.Addr
	// Wake is every suspended service replica this host holds a reserved slot
	// for. Traffic addressed to one is counted and dropped, and the count is
	// what brings the machine back -- the mechanism that lets a service scale
	// to zero and stay reachable by name.
	Wake []WakeTarget
}

// Fingerprint is a stable summary of the desired state.
//
// The caller applies rules only when this changes. The loop that drives it
// ticks far more often than the fleet moves, and rebuilding the table costs a
// netlink transaction proportional to the number of machines on the host --
// paid several times a second, forever, to arrive at the rules already there.
func (r TenantRules) Fingerprint() string {
	h := sha256.New()
	for _, m := range r.Local {
		fmt.Fprintf(h, "L|%d|%s|%s\n", m.SlotIdx, m.Addr, m.App)
	}
	apps := make([]string, 0, len(r.Apps))
	for app := range r.Apps {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		addrs := append([]netip.Addr(nil), r.Apps[app]...)
		sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
		fmt.Fprintf(h, "A|%s|", app)
		for _, a := range addrs {
			fmt.Fprintf(h, "%s,", a)
		}
		fmt.Fprintln(h)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ApplyTenantFilter makes the root namespace's rules match r, exactly.
//
// Delete-and-rebuild rather than a diff. The rules are a function of fleet
// state with no per-flow state of their own, the whole thing is one netlink
// transaction the kernel applies atomically, and a diff over sets and their
// elements would be considerably more code for a path that runs when a machine
// is created or moves.
func ApplyTenantFilter(r TenantRules) error {
	if err := os.WriteFile(v6ForwardingKnob, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("netns: enable ipv6 forwarding: %w", err)
	}

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("netns: nftables conn: %w", err)
	}
	defer c.CloseLasting()

	// Remove the previous generation first, in the SAME batch. Left in place,
	// the old table's rules keep matching alongside the new ones and a machine
	// that has been destroyed keeps its reachability.
	existing, err := c.ListTables()
	if err != nil {
		return fmt.Errorf("netns: list tables: %w", err)
	}
	for _, t := range existing {
		if t.Name == tenantTable && t.Family == nftables.TableFamilyINet {
			c.DelTable(t)
		}
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: tenantTable})
	chain := c.AddChain(&nftables.Chain{
		Name: "forward", Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   chainPolicy(nftables.ChainPolicyAccept),
	})

	// Belt and braces with the delete above, which is load-bearing enough to
	// be worth stating twice: everything below appends, so a chain that
	// survives this batch with its previous generation intact silently
	// becomes the union of two generations rather than the newer one. The
	// delete should prevent that on its own. This costs one netlink message
	// and removes the need to be right about that.
	c.FlushChain(chain)

	// Return traffic first, before any drop can catch it -- and before the
	// per-machine rules, so an established conversation costs one lookup
	// rather than a walk over every machine on the host.
	c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryU32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:  binaryU32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryU32(0)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})

	sets, err := addAppSets(c, table, r.Apps)
	if err != nil {
		return err
	}

	for _, m := range r.Local {
		if m.SlotIdx <= 0 {
			continue
		}
		veth := VEthNameFor(m.SlotIdx)
		set := sets[m.App]

		// A guest may not put another machine's address in its source. It
		// cannot reach a peer that way -- the reply would go to the real
		// owner -- but it could forge a connection attempt that the peer's
		// own filter would then accept as coming from inside the app.
		if m.Addr.IsValid() {
			c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: concat(
				matchIface(expr.MetaKeyIIFNAME, veth),
				matchProto6(),
				payload6(srcOffset6),
				[]expr.Any{
					&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: m.Addr.AsSlice()},
					&expr.Verdict{Kind: expr.VerdictDrop},
				},
			)})
		}

		// Its own app, and nothing else. A machine with no app has no set and
		// falls straight through to the drop below, which is the right default
		// for a sandbox that was never told it belongs anywhere.
		if set != nil {
			c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: concat(
				matchIface(expr.MetaKeyIIFNAME, veth),
				matchProto6(),
				payload6(dstOffset6),
				[]expr.Any{
					&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},
					&expr.Verdict{Kind: expr.VerdictAccept},
				},
			)})
		}

		// Everything else this guest sends over IPv6 is refused HERE, at the
		// network layer. That includes fdcc::/16, where hostd's internal
		// listener sits: it is bearer-authenticated, so this is depth rather
		// than the only door, but a leaked API key must not become fleet-wide
		// exec and a 401 would prove only that auth was awake.
		//
		// IPv4 is untouched by the nfproto guard, so a guest's egress to the
		// internet keeps working exactly as before.
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: concat(
			matchIface(expr.MetaKeyIIFNAME, veth),
			matchProto6(),
			[]expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}},
		)})

		// The inbound half. The sender's own host should already have dropped
		// this, so reaching here means that host is running something other
		// than these rules -- which is precisely when a second check is worth
		// having.
		inbound := concat(
			matchIface(expr.MetaKeyOIFNAME, veth),
			matchProto6(),
		)
		if set != nil {
			inbound = concat(inbound, payload6(srcOffset6), []expr.Any{
				&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID, Invert: true},
			})
		}
		c.AddRule(&nftables.Rule{Table: table, Chain: chain,
			Exprs: concat(inbound, []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}})})
	}

	applyWakeRules(c, table, r.Wake)

	if err := c.Flush(); err != nil {
		return fmt.Errorf("netns: apply the tenant filter: %w", err)
	}
	return nil
}

// addAppSets creates one address set per app.
func addAppSets(c *nftables.Conn, table *nftables.Table,
	apps map[string][]netip.Addr) (map[string]*nftables.Set, error) {

	names := make([]string, 0, len(apps))
	for app := range apps {
		if app != "" {
			names = append(names, app)
		}
	}
	// Sorted so the rule set is byte-identical for identical fleet state,
	// which is what makes the fingerprint above meaningful.
	sort.Strings(names)

	out := make(map[string]*nftables.Set, len(names))
	for _, app := range names {
		set := &nftables.Set{
			Table: table, Name: appSetName(app), KeyType: nftables.TypeIP6Addr,
		}
		elements := make([]nftables.SetElement, 0, len(apps[app]))
		seen := map[netip.Addr]bool{}
		for _, addr := range apps[app] {
			if !addr.Is6() || seen[addr] {
				continue
			}
			seen[addr] = true
			elements = append(elements, nftables.SetElement{Key: addr.AsSlice()})
		}
		sort.Slice(elements, func(i, j int) bool {
			return string(elements[i].Key) < string(elements[j].Key)
		})
		if err := c.AddSet(set, elements); err != nil {
			return nil, fmt.Errorf("netns: set for app %q: %w", app, err)
		}
		out[app] = set
	}
	return out, nil
}

// appSetName is a hash, not the app name.
//
// An app name comes from a client's compose file: it can be long, and it can
// contain bytes nftables will not take in an identifier. Hashing gives a name
// that is always valid and always the same length, and the set is never read
// by a human looking for a particular app -- it is read by the rule that
// references it.
func appSetName(app string) string {
	sum := sha256.Sum256([]byte(app))
	return "app-" + hex.EncodeToString(sum[:6])
}

// matchIface matches an ingress or egress interface by name.
func matchIface(key expr.MetaKey, name string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(name)},
	}
}

// matchProto6 restricts a rule to IPv6, leaving every guest's IPv4 egress
// exactly as it was.
func matchProto6() []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
	}
}

// payload6 loads an IPv6 address from the network header into register 1.
func payload6(offset uint32) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader,
			Offset: offset, Len: 16},
	}
}

// ifname is an interface name as the kernel compares it: a fixed 16 bytes,
// NUL-padded. A shorter comparison would match every interface sharing the
// prefix, so veth-1 would match veth-10.
func ifname(name string) []byte {
	out := make([]byte, unix.IFNAMSIZ)
	copy(out, name)
	return out
}

func concat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
