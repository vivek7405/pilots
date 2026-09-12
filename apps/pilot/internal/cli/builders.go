package cli

import (
	"strconv"

	"github.com/spf13/cobra"
)

// An org's build machines, and the reset for when one is wrong.
//
// A builder is a machine pilots makes for you: one per org per host, exempt
// from your quota, not routable, created the first time you build on that
// host. You do not manage it, which is why it is absent from `pilot machines
// ls`. What you occasionally need is to see that it exists and to throw it
// away, and the second one is the older of the two requests any build service
// gets.

func newBuildersCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "builders",
		Short: "the build machines pilots runs for your org",
		Args:  cobra.NoArgs,
	}
	Describe(c, Doc{
		When: "Your builds run inside a machine of your own, one per host you have\n" +
			"built on. They are created for you and are not in `pilot machines ls`.\n" +
			"Come here when a build behaves as though it is reading something\n" +
			"stale, or when you want to see what the platform is running for you.",
		How: "`reset` destroys the builder on one host AND drops your layer cache\n" +
			"everywhere. The next build on any host starts cold and correct.\n" +
			"Nothing is broadcast to the other hosts: each finds out when it\n" +
			"next builds.",
		Examples: []string{
			"pilot builders ls",
			"pilot builders reset host-a",
		},
		Related: []string{
			"pilot deploy   builds and runs; this is where those builds happen",
		},
	})
	c.AddCommand(newBuildersListCmd(env), newBuildersResetCmd(env))
	return c
}

func newBuildersListCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list your org's builders",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			builders, err := client.Builders.List(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(builders)
			}
			rows := make([][]string, 0, len(builders))
			for _, b := range builders {
				rows = append(rows, []string{
					b.HostID, b.State,
					strconv.Itoa(b.VCPUs) + " vCPU / " + strconv.Itoa(b.MemMiB) + " MiB",
					b.ID,
				})
			}
			return env.W.Table([]string{"HOST", "STATE", "SIZE", "ID"}, rows)
		},
	}
	Describe(c, Doc{
		How: "One row per host you have built on. An empty list means no build\n" +
			"has run for this org yet, which is the normal state of a new org.",
		Examples: []string{"pilot builders ls"},
	})
	return c
}

func newBuildersResetCmd(env *Env) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "reset <host>",
		Short: "destroy a builder and clear the layer cache",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			host := args[0]
			if !yes {
				if err := confirm(env, "reset the builder on "+host+
					" and clear this org's layer cache"); err != nil {
					return err
				}
			}
			res, err := client.Builders.Reset(c.Context(), host)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}
			env.W.Linef("reset the builder on %s; the next build everywhere starts cold", host)
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	Describe(c, Doc{
		How: "Two things at once, because they answer two different complaints:\n" +
			"the builder on this host is destroyed and recreated by your next\n" +
			"build, and your layer cache generation moves so every OTHER host\n" +
			"drops its copy as well. The next build is slower and correct.",
		Examples: []string{"pilot builders reset host-a", "pilot builders reset host-a -y"},
	})
	return c
}
