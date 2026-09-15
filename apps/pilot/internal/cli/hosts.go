package cli

import (
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// The fleet, and emptying one host of it on purpose.
//
// `pilot status` already prints the host table, and it stays: this is the
// place operator ACTIONS on a host live, which status is not.

func newHostsCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "hosts",
		Aliases: []string{"host"},
		Short:   "the fleet's hosts, and draining one",
	}
	Describe(c, Doc{
		What: "Every host runs the identical stack and serves the whole API, so there\n" +
			"is no host to prefer and none to protect. What a host does have is\n" +
			"machines on it, and `drain` is how they leave before it does.",
		Related: []string{
			"pilot status   the fleet at a glance, hosts included",
		},
	})
	c.AddCommand(newHostsListCmd(env), newHostsDrainCmd(env), newHostsUndrainCmd(env))
	return c
}

func newHostsListCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "the hosts in this fleet",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			hosts, err := client.Hosts.List(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(hosts)
			}
			rows := make([][]string, 0, len(hosts))
			for _, h := range hosts {
				state := "-"
				if h.Draining {
					state = "draining"
				}
				rows = append(rows, []string{
					h.ID, strconv.FormatBool(h.Alive),
					strconv.Itoa(h.MemFreeMiB), strconv.Itoa(h.MemReclaimableMiB),
					strconv.Itoa(h.VCPUsRunning), orDash(h.CPUVendor), state,
				})
			}
			return env.W.Table([]string{
				"HOST", "ALIVE", "MEM FREE MIB", "RECLAIMABLE MIB", "VCPUS RUNNING", "CPU", "STATE",
			}, rows)
		},
	}
	Describe(c, Doc{
		How: "RECLAIMABLE is memory held by running machines the host would suspend\n" +
			"if it needed the room. Placement counts it as available, so a host\n" +
			"whose free column looks small can still take a machine.",
		Examples: []string{"pilot hosts ls", "pilot hosts ls --json"},
	})
	return c
}

func newHostsDrainCmd(env *Env) *cobra.Command {
	var wait bool
	c := &cobra.Command{
		Use:   "drain <host>",
		Short: "move every machine off a host",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			report, err := client.Hosts.Drain(c.Context(), args[0])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(report)
			}
			env.W.Linef("moved %d machine(s) off %s", len(report.Moved), args[0])
			for _, id := range report.Left {
				env.W.Notef("%s stayed: %s", id, report.Errors[id])
			}
			if !wait || len(report.Left) == 0 {
				if len(report.Left) > 0 {
					return out.Failf("add a host, or free memory elsewhere, then drain again",
						"%d machine(s) could not be moved", len(report.Left))
				}
				return nil
			}
			// Keep asking until it is empty. A drain that left machines behind
			// usually did so because no host had room at that moment, and room
			// appears as other machines idle out.
			deadline := time.Now().Add(10 * time.Minute)
			for time.Now().Before(deadline) {
				status, err := client.Hosts.DrainStatus(c.Context(), args[0])
				if err != nil {
					return err
				}
				if len(status.Left) == 0 {
					env.W.Linef("%s is empty", args[0])
					return nil
				}
				time.Sleep(5 * time.Second)
			}
			return out.Failf("pilot hosts drain "+args[0]+" again, or check capacity with pilot hosts ls",
				"%s still holds machines after ten minutes", args[0])
		},
	}
	c.Flags().BoolVar(&wait, "wait", false, "keep checking until the host is empty")
	Describe(c, Doc{
		How: "Each machine is suspended here and restored on another host. Its id,\n" +
			"its name and its URL do not change, so nothing a user holds stops\n" +
			"working: a request arriving mid-move is HELD and served late, never\n" +
			"refused.\n\n" +
			"Suspended machines move first, because moving one is a row write and\n" +
			"nothing else. Machines with a VOLUME move last and take longest: a\n" +
			"volume has one writer, so the replacement cannot mount it until this\n" +
			"host has let go.\n\n" +
			"The host keeps refusing new machines afterwards, which is the point --\n" +
			"it is about to be rebooted. `pilot hosts undrain` lets it take work\n" +
			"again.",
		Warning: "A machine no other host can hold stays here and is reported. Drain\n" +
			"does not destroy anything to make room.",
		Examples: []string{
			"pilot hosts drain host-3",
			"pilot hosts drain host-3 --wait",
		},
		Related: []string{
			"pilot hosts undrain   let it take machines again",
			"pilot hosts ls        what each host has free",
		},
	})
	return c
}

func newHostsUndrainCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "undrain <host>",
		Short: "let a drained host take machines again",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			if err := client.Hosts.Undrain(c.Context(), args[0]); err != nil {
				return err
			}
			env.W.Linef("%s takes machines again", args[0])
			return nil
		},
	}
	Describe(c, Doc{
		How: "Nothing moves BACK. The machines that left are where they are, and\n" +
			"moving them a second time for tidiness would be a second outage for\n" +
			"no benefit. What changes is that placement will consider this host\n" +
			"again.",
		Examples: []string{"pilot hosts undrain host-3"},
	})
	return c
}
