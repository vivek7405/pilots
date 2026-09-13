package cli

import (
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// What a machine is running, and how to bounce one of it.
//
// A machine runs a named SET of processes rather than one anonymous command:
// an image's own CMD is the process `app`, and a compose file or a runtime
// registration adds more. The names are the whole point. Restarting the dev
// server in a machine must not take down the database beside it, and reading
// "which of my three processes is crash-looping" should not mean scrolling a
// console log where all three are interleaved.

func newProcessesCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "processes",
		Aliases: []string{"ps"},
		Short:   "the processes running inside a machine",
		Args:    cobra.NoArgs,
	}
	Describe(c, Doc{
		When: "A machine runs a named set of processes. An image's own command is\n" +
			"`app`; a compose file with two services on one build context adds a\n" +
			"second; anything started through the agent at runtime adds its own.",
		How: "`ls` shows every process with its state, pid and restart count.\n" +
			"`start`, `stop` and `restart` act on ONE of them and leave the rest\n" +
			"alone. `logs` is that process's own output, not the machine's.",
		Examples: []string{
			"pilot processes ls api",
			"pilot processes restart api worker",
			"pilot processes logs api worker --tail 50",
		},
		Related: []string{
			"pilot logs      the machine's console, all processes interleaved",
			"pilot machines restart   restart every process in a machine",
		},
	})
	c.AddCommand(
		newProcessesListCmd(env),
		newProcessActionCmd(env, "start", "start a stopped process"),
		newProcessActionCmd(env, "stop", "stop one process and leave it stopped"),
		newProcessActionCmd(env, "restart", "restart one process"),
		newProcessLogsCmd(env),
	)
	return c
}

func newProcessesListCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "list [machine]",
		Aliases: []string{"ls"},
		Short:   "list a machine's processes",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := machineArg(c, env, client, first(args))
			if err != nil {
				return err
			}
			procs, err := client.Machines.Processes(c.Context(), m.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(procs)
			}
			rows := make([][]string, 0, len(procs))
			for _, p := range procs {
				pid := ""
				if p.PID > 0 {
					pid = strconv.Itoa(p.PID)
				}
				name := p.Name
				if p.Port {
					// The one process the machine's URL reaches. Worth marking,
					// because "my app is not serving" is usually a question
					// about which process holds the port.
					name += " *"
				}
				rows = append(rows, []string{
					name, p.State, pid, strconv.Itoa(p.Restarts), p.Cmd,
				})
			}
			return env.W.Table([]string{"NAME", "STATE", "PID", "RESTARTS", "COMMAND"}, rows)
		},
	}
	Describe(c, Doc{
		How: "A star marks the process that owns the machine's port. RESTARTS\n" +
			"climbing on its own is a crash loop; `logs` for that process says why.",
		Examples: []string{"pilot processes ls api"},
	})
	return c
}

func newProcessActionCmd(env *Env, verb, short string) *cobra.Command {
	c := &cobra.Command{
		Use:   verb + " [machine] <process>",
		Short: short,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			// The process name is always the LAST argument, so both
			// `pilot processes restart worker` (machine from context) and
			// `pilot processes restart api worker` read the same way.
			name := args[len(args)-1]
			var machine string
			if len(args) == 2 {
				machine = args[0]
			}
			m, err := machineArg(c, env, client, machine)
			if err != nil {
				return err
			}
			ctx := c.Context()
			switch verb {
			case "start":
				err = client.Machines.StartProcess(ctx, m.ID, name)
			case "stop":
				err = client.Machines.StopProcess(ctx, m.ID, name)
			case "restart":
				err = client.Machines.RestartProcess(ctx, m.ID, name)
			}
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{verb: name, "machine": m.ID})
			}
			env.W.Linef("%s %s on %s", verb, name, m.ID)
			return nil
		},
	}
	Describe(c, Doc{
		How: "Acts on ONE process. Every other process in the machine keeps its\n" +
			"pid, which is the reason processes have names.",
		Examples: []string{"pilot processes " + verb + " api worker"},
	})
	return c
}

func newProcessLogsCmd(env *Env) *cobra.Command {
	var tail int
	c := &cobra.Command{
		Use:   "logs [machine] <process>",
		Short: "one process's output",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			name := args[len(args)-1]
			var machine string
			if len(args) == 2 {
				machine = args[0]
			}
			m, err := machineArg(c, env, client, machine)
			if err != nil {
				return err
			}
			out, err := client.Machines.ProcessLogs(c.Context(), m.ID, name, tail)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"machine": m.ID, "process": name, "logs": out})
			}
			_, err = io.WriteString(os.Stdout, out)
			return err
		},
	}
	c.Flags().IntVar(&tail, "tail", 0, "show only the last N lines")
	Describe(c, Doc{
		How: "This process's own output, kept in a bounded buffer inside the\n" +
			"guest. Everything is also on the machine's console, where all the\n" +
			"processes are interleaved; this is the one you asked for.",
		Examples: []string{"pilot processes logs api worker --tail 50"},
	})
	return c
}
