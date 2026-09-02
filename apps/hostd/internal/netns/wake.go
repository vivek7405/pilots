package netns

import (
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// wakeChain holds one counted drop per suspended service replica.
//
// A suspended machine has no namespace and no process, so a packet addressed
// to it dies in the root namespace with nowhere to go. That is correct and it
// is also the signal: something wanted this machine. Counting the drop turns
// "traffic arrived for a machine that is not running" into a wake trigger,
// which is what lets a service scale to zero and still be reachable by a peer
// over .internal.
//
// Deliberately a counter read on the existing reconcile tick rather than an
// NFQUEUE handed to userspace. The queue would wake fractionally sooner and
// costs a kernel-userspace packet path, a queue-number allocation, and a
// reader whose death silently stops every wake in the fleet. The tick already
// runs, already writes these rules, and a wake that takes up to one tick
// longer is a wake the client's own TCP retransmission absorbs.
const wakeChain = "wake"

// WakeTarget is a suspended machine that traffic should bring back.
type WakeTarget struct {
	MachineID string
	Addr      netip.Addr
}

// applyWakeRules replaces the wake chain with one counted rule per target.
//
// Rebuilt wholesale on every pass, like the tenant rules beside it: the set of
// suspended replicas changes as machines come and go, and a stale rule would
// either count for a machine that is running again or miss one that just
// stopped.
func applyWakeRules(c *nftables.Conn, table *nftables.Table, targets []WakeTarget) {
	chain := c.AddChain(&nftables.Chain{
		Name: wakeChain, Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward,
		// After the tenant filter. A packet that the tenant boundary refuses
		// must not wake anything -- otherwise anyone who can address a machine
		// can bill its owner for a resume.
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityFilter + 10),
		Policy:   chainPolicy(nftables.ChainPolicyAccept),
	})
	c.FlushChain(chain)

	for _, t := range targets {
		if !t.Addr.IsValid() {
			continue
		}
		// Counted, then dropped. Dropping is not a policy decision: there is
		// nothing behind the address until the machine is back, so the packet
		// has nowhere to go either way. The client retransmits and lands on a
		// running machine.
		c.AddRule(&nftables.Rule{
			Table: table, Chain: chain,
			UserData: []byte(t.MachineID),
			Exprs: concat(
				matchProto6(),
				payload6(dstOffset6),
				[]expr.Any{
					&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: t.Addr.AsSlice()},
					&expr.Counter{},
					&expr.Verdict{Kind: expr.VerdictDrop},
				},
			),
		})
	}
}

// WakeCounts reads how many packets have arrived for each suspended machine.
//
// Keyed by machine id, which travels in the rule's UserData -- the kernel
// hands rules back in the order they were added, but matching on position
// would break the moment a machine is added or removed between passes.
func WakeCounts() (map[string]uint64, error) {
	c, err := nftables.New()
	if err != nil {
		return nil, err
	}
	defer c.CloseLasting()

	tables, err := c.ListTables()
	if err != nil {
		return nil, err
	}
	var table *nftables.Table
	for _, t := range tables {
		if t.Name == tenantTable && t.Family == nftables.TableFamilyINet {
			table = t
			break
		}
	}
	if table == nil {
		return nil, nil
	}

	rules, err := c.GetRules(table, &nftables.Chain{Name: wakeChain, Table: table})
	if err != nil {
		// A fleet that has never suspended a service replica has no chain, and
		// that is not an error worth logging every tick.
		return nil, nil
	}

	out := make(map[string]uint64, len(rules))
	for _, r := range rules {
		id := string(r.UserData)
		if id == "" {
			continue
		}
		for _, e := range r.Exprs {
			if ctr, ok := e.(*expr.Counter); ok {
				out[id] = ctr.Packets
			}
		}
	}
	return out, nil
}
