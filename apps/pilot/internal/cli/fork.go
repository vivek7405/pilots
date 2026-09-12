package cli

import (
	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// newMachinesForkCmd makes new machines out of an existing one's exact state.
//
// The command this platform is for. A plain create starts an application from
// nothing; a fork starts it from wherever another machine already got to.
func newMachinesForkCmd(env *Env) *cobra.Command {
	var (
		count  int
		name   string
		volume bool
	)
	c := &cobra.Command{
		Use:   "fork <machine|checkpoint>",
		Short: "new machines from an existing one's exact state",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			req := pilots.ForkRequest{Name: name, Count: count, Volume: volume}

			// A checkpoint id is forked through its own route. Resolving the
			// argument as a machine FIRST means a name always wins, which is
			// what a person typing one expects.
			var res *pilots.ForkResponse
			m, merr := resolveMachine(c.Context(), client, args[0])
			if merr == nil {
				res, err = client.Machines.Fork(c.Context(), m.ID, req)
			} else {
				res, err = client.Checkpoints.Fork(c.Context(), args[0], req)
			}
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}

			rows := make([][]string, 0, len(res.Forks))
			failed := 0
			for _, f := range res.Forks {
				if f.Machine == nil {
					failed++
					rows = append(rows, []string{"-", "-", f.Error})
					continue
				}
				rows = append(rows, []string{f.Machine.Name, f.Machine.ID, f.Machine.URL})
			}
			if err := env.W.Table([]string{"NAME", "ID", "URL"}, rows); err != nil {
				return err
			}
			if failed > 0 {
				return out.Failf("try again for the ones that failed; the others are running",
					"%d of %d forks did not come up", failed, len(res.Forks))
			}
			return nil
		},
	}
	f := c.Flags()
	f.IntVar(&count, "count", 1, "how many forks to make, up to 100")
	f.StringVar(&name, "name", "", "name the first fork; the rest take a suffix")
	f.BoolVar(&volume, "volume", false, "fork the source's volume too")
	Describe(c, Doc{
		What: "A fork is a NEW machine -- new id, new name, new URL -- restored from\n" +
			"another machine's memory and disk. It comes up with the source's\n" +
			"processes already running and its memory already warm.",
		When: "When getting to a state is the expensive part. An agent that spent two\n" +
			"minutes installing dependencies and loading a model forks into ten\n" +
			"machines that all begin from that moment.",
		How: "A RUNNING source is checkpointed first, in place: it keeps its id, its\n" +
			"URL and its slot, and the checkpoint is the exact moment every fork\n" +
			"begins from.\n\n" +
			"A SUSPENDED source is forked WITHOUT being woken. Its suspend image is\n" +
			"already a restorable point, so forking a sleeping machine costs it\n" +
			"nothing.\n\n" +
			"Forks are independent: if one fails the others still come up, and the\n" +
			"table says which.",
		Warning: "A source with a VOLUME needs --volume. Without it the fork is refused\n" +
			"rather than started with memory expecting a disk it does not have.",
		Examples: []string{
			"pilot machines fork agent-base",
			"pilot machines fork agent-base --count 10",
			"pilot machines fork ck_abc123 --name trial",
		},
		Related: []string{
			"pilot machines checkpoint   record a moment to fork from later",
			"pilot machines restore      put a machine BACK to a checkpoint, in place",
		},
	})
	return c
}
