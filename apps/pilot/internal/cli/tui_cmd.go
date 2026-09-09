package cli

import (
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vivek7405/pilots/cli/internal/out"
	"github.com/vivek7405/pilots/cli/internal/tui"
)

func newTUICmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "tui",
		Aliases: []string{"dashboard", "ui"},
		Short:   "the fleet as a dashboard you leave open",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !term.IsTerminal(int(os.Stdout.Fd())) {
				return out.Failf("run it from a terminal; `pilot status --watch` is the piped form", "the dashboard needs a terminal")
			}
			client, err := env.Client()
			if err != nil {
				return err
			}
			// The dashboard hands a console request back rather than opening
			// one itself, so raw-mode handling lives in exactly one place.
			// When the shell ends, the dashboard comes back where it was.
			for {
				exit, err := tui.Run(c.Context(), client)
				if err != nil {
					return err
				}
				if exit.ConsoleMachine == "" {
					return nil
				}
				if err := runConsole(c, env, client, exit.ConsoleMachine, nil); err != nil {
					var ee *ExitError
					if !asExit(err, &ee) {
						env.W.Notef("console: %v", err)
					}
				}
			}
		},
	}
	Describe(c, Doc{
		What: "Hosts, machines and services on one screen, refreshed every two\n" +
			"seconds, with a minute of history in the cards. Open a machine or a\n" +
			"service for its details and replicas, follow its logs, and act on\n" +
			"it from the keyboard: suspend, wake, checkpoint, promote, scale,\n" +
			"roll back, destroy. `c` drops into a console and returns when the\n" +
			"shell exits. Press ? inside for every key.",
		How: "The palette follows the terminal's background: it asks the terminal\n" +
			"whether it is light or dark rather than making you pick a theme.",
		Examples: []string{"pilot tui", "pilot dashboard"},
		Related: []string{
			"pilot status --watch   the same numbers for a pipe or a small terminal",
		},
	})
	return c
}

func asExit(err error, target **ExitError) bool {
	e, ok := err.(*ExitError)
	if ok {
		*target = e
	}
	return ok
}
