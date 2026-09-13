package cli

import "github.com/spf13/cobra"

var egressHeaders = []string{"HOST", "IPV6", "INTERFACE"}

// newEgressCmd prints the addresses this org's outbound traffic leaves from.
//
// The command exists because the answer is otherwise unknowable: a tenant
// integrating with anything that allowlists by source address has to be told
// what to give them, and no amount of looking at their own machines reveals
// it.
func newEgressCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "egress",
		Short: "the addresses this org's outbound traffic leaves from",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			res, err := client.Hosts.Egress(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}
			if len(res.Addresses) == 0 {
				env.W.Notef("no host in this fleet hands out per-org egress addresses, " +
					"so your traffic leaves from each host's shared address")
				return nil
			}
			rows := make([][]string, 0, len(res.Addresses))
			for _, a := range res.Addresses {
				rows = append(rows, []string{a.HostID, a.IPv6, a.Interface})
			}
			return env.W.Table(egressHeaders, rows)
		},
	}
	Describe(c, Doc{
		How: "One address per host, because each host derives it from its own routed\n" +
			"prefix. Allowlist the WHOLE set: a machine can be created on, moved to,\n" +
			"or rescued onto any host in the fleet.\n\n" +
			"The set changes only when a host joins or leaves. It does NOT change when\n" +
			"your machines are created, destroyed, resized, rolled or moved, which is\n" +
			"why the address is per org rather than per machine.\n\n" +
			"IPv6 only. An IPv4 address is purchased and scarce and a bare-metal host\n" +
			"has one, so v4 traffic shares the host's address and always will.",
		Examples: []string{
			"pilot egress",
			"pilot egress --json",
		},
	})
	return c
}
