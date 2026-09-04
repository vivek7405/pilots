package netns

import (
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// activityChain holds one counted rule per RUNNING service replica.
//
// The mirror of the wake chain. A suspended replica's packets are counted and
// dropped, and the count wakes it; a running replica's packets are counted
// and let through, and the count is what tells the autoscaler the replica is
// in use by a peer. Without it a database serving only .internal clients is
// idle to every signal the autoscaler reads, which is the failure Fly's
// Postgres images work around by counting their own connections.
//
// After the tenant filter, like the wake chain: a packet the boundary refused
// must not count as activity. No verdict: the counter observes and the packet
// continues exactly as it would have.
const activityChain = "activity"

func applyActivityRules(c *nftables.Conn, table *nftables.Table, targets []WakeTarget) {
	chain := c.AddChain(&nftables.Chain{
		Name: activityChain, Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward,
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityFilter + 11),
		Policy:   chainPolicy(nftables.ChainPolicyAccept),
	})
	c.FlushChain(chain)

	for _, t := range targets {
		if !t.Addr.IsValid() {
			continue
		}
		c.AddRule(&nftables.Rule{
			Table: table, Chain: chain,
			UserData: []byte(t.MachineID),
			Exprs:    activityExprs(t.Addr),
		})
	}
}

// activityExprs counts packets to addr and decides nothing.
func activityExprs(addr netip.Addr) []expr.Any {
	return concat(
		matchProto6(),
		payload6(dstOffset6),
		[]expr.Any{
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: addr.AsSlice()},
			&expr.Counter{},
		},
	)
}

// ActivityCounts reads how many packets have arrived for each running replica.
func ActivityCounts() (map[string]uint64, error) { return chainCounts(activityChain) }
