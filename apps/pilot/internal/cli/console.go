package cli

import (
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

func newConsoleCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "console [machine] [-- command...]",
		Aliases: []string{"c"},
		Short:   "an interactive shell on a machine",
		Args:    cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			name, argv := splitAtDash(c, args)
			m, err := machineArg(c, env, client, name)
			if err != nil {
				return err
			}
			return runConsole(c, env, client, m.ID, argv)
		},
	}
	Describe(c, Doc{
		When: "- `pilot console` -- a shell to work in, on a pseudo-terminal\n" +
			"- `pilot exec`    -- one command, its output back, its exit status yours\n\n" +
			"Reach for console when you want to look around or run several things;\n" +
			"for exec when a script or an agent wants an answer.",
		How: "The window size is forwarded, so full-screen programs lay out\n" +
			"correctly and `stty size` in the guest agrees with your terminal.\n" +
			"Everything after -- replaces the default `/bin/sh -l`. Exiting the\n" +
			"shell ends the session; the machine keeps running.",
		Examples: []string{
			"pilot console scratch",
			"pilot c scratch",
			"pilot console scratch -- bash",
			"pilot console scratch -- htop",
		},
		Related: []string{
			"pilot exec              one command, output back",
			"pilot machines create   creates a machine and opens a console on it",
		},
	})
	return c
}

// newExecCmd is the top-level `pilot exec`, the same as `pilot machines
// exec`. sprite puts `exec (x)` and `console (c)` at the top level because
// they are the two commands an agent runs a hundred times a session; making
// them a level deeper would cost a word each time for no gain.
func newExecCmd(env *Env) *cobra.Command {
	c := newMachinesExecCmd(env)
	c.Use = "exec <machine> -- <command> [args...]"
	return c
}

// runConsole runs argv (or a login shell) on a TTY, with this terminal in raw
// mode so keystrokes reach the guest unedited and the guest's output reaches
// the screen unfiltered.
func runConsole(c *cobra.Command, env *Env, client *pilots.Client, id string, argv []string) error {
	stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(stdinFd) || !term.IsTerminal(stdoutFd) {
		return out.Failf("run it from a terminal, or use `pilot exec` for a command without one",
			"a console needs stdin and stdout to be a terminal")
	}
	if len(argv) == 0 {
		argv = []string{"/bin/sh", "-l"}
	}

	cols, rows, err := term.GetSize(stdoutFd)
	if err != nil {
		cols, rows = 80, 24
	}
	stream, err := client.Machines.ExecStream(c.Context(), id, argv, pilots.ExecStreamOptions{
		Stdin: true, TTY: true, Cols: uint16(cols), Rows: uint16(rows),
	})
	if err != nil {
		return err
	}
	// Raw mode, window size, ctrl-\ to detach: the same loop `pilot attach`
	// runs, because a console IS a session from its first frame.
	return driveTerminal(env, stream, stdinFd, stdoutFd, "")
}
