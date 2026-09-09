package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

var machineHeaders = []string{"NAME", "STATE", "HOST", "URL", "ID"}

func machineRow(m *pilots.Machine) []string {
	return []string{m.Name, m.State, m.HostID, m.URL, m.ID}
}

// parseEnv turns repeated K=V flags into a map, refusing a value with no `=`
// rather than silently making it an empty variable.
func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, out.Failf("write it as KEY=value", "--env %q is not KEY=value", p)
		}
		env[k] = v
	}
	return env, nil
}

func unixTime(t int64) string {
	if t == 0 {
		return "-"
	}
	return time.Unix(t, 0).Local().Format("2006-01-02 15:04:05")
}

func newMachinesCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "machines",
		Aliases: []string{"machine", "m"},
		Short:   "create and drive machines",
	}
	Describe(c, Doc{
		What: "A machine is one Firecracker microVM: its own kernel, its own disk,\n" +
			"its own URL. It is the only primitive here. Left alone it is a\n" +
			"sandbox; promoted it is a production service replica. Its URL never\n" +
			"changes -- not on suspend, wake, checkpoint, restore or promote.",
		Related: []string{
			"pilot promote   turn a machine into a service, keeping its URL",
			"pilot deploy    build a directory and run it as a service",
			"pilot console   an interactive shell on a machine",
		},
	})
	c.AddCommand(
		newMachinesListCmd(env),
		newMachinesCreateCmd(env),
		newMachinesInfoCmd(env),
		newMachinesDestroyCmd(env),
		newMachinesLifecycleCmd(env, "start", "start a stopped machine", "Start", "A stopped machine boots again; a suspended one wakes instantly."),
		newMachinesLifecycleCmd(env, "stop", "stop a machine", "Stop", "Stopping releases CPU and memory. The disk stays. `start` boots it again."),
		newMachinesLifecycleCmd(env, "suspend", "suspend a machine to a memory snapshot", "Suspend", "Suspend captures memory so `wake` resumes where it left off, instantly.\nThis is what auto_stop does on idle."),
		newMachinesLifecycleCmd(env, "wake", "wake a suspended machine", "Wake", "A request to the machine's URL wakes it on its own; this does it by hand."),
		newMachinesExecCmd(env),
		newMachinesLogsCmd(env),
		newMachinesCheckpointCmd(env),
		newMachinesCheckpointsCmd(env),
		newMachinesRestoreCmd(env),
	)
	return c
}

func newMachinesListCmd(env *Env) *cobra.Command {
	var prefix string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list machines",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			machines, err := client.Machines.List(c.Context())
			if err != nil {
				return err
			}
			if prefix != "" {
				kept := machines[:0]
				for _, m := range machines {
					if strings.HasPrefix(m.Name, prefix) {
						kept = append(kept, m)
					}
				}
				machines = kept
			}
			if env.W.JSON {
				if machines == nil {
					machines = []pilots.Machine{}
				}
				return env.W.JSONValue(machines)
			}
			rows := make([][]string, 0, len(machines))
			for i := range machines {
				rows = append(rows, machineRow(&machines[i]))
			}
			if len(rows) == 0 {
				// An empty list is an answer, not an error; it goes to
				// stderr so `| wc -l` still counts zero rows.
				env.W.Notef("no machines in %s", orgLabel(env))
				return nil
			}
			return env.W.Table(machineHeaders, rows)
		},
	}
	c.Flags().StringVar(&prefix, "prefix", "", "only machines whose name starts with this")
	Describe(c, Doc{
		When: "To see what exists. For one machine in detail, `pilot machines info`.",
		Examples: []string{
			"pilot machines ls",
			"pilot m ls --prefix agent-",
			"# every field, for a script",
			"pilot machines ls --json | jq -r '.[] | .name'",
		},
	})
	return c
}

func newMachinesCreateCmd(env *Env) *cobra.Command {
	var (
		req         pilots.CreateMachineRequest
		envPairs    []string
		skipConsole bool
	)
	c := &cobra.Command{
		Use:   "create [name]",
		Short: "create a machine and open a console on it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				req.Name = args[0]
			}
			if req.Env, err = parseEnv(envPairs); err != nil {
				return err
			}
			m, err := client.Machines.Create(c.Context(), req)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(m)
			}
			if err := env.W.Table(machineHeaders, [][]string{machineRow(m)}); err != nil {
				return err
			}
			// Straight into a shell, the way sprite does: the reason to
			// create a sandbox is almost always to use it, and a second
			// command to get there is a papercut every time. Not when the
			// answer is being piped, and not when asked not to.
			if skipConsole || !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
				return nil
			}
			env.W.Notef("connecting to %s; exit the shell to detach", m.Name)
			return runConsole(c, env, client, m.ID, nil)
		},
	}
	f := c.Flags()
	f.StringVar(&req.Image, "image", "", "a rootfs build id from `pilot deploy` or the build tool")
	f.StringVar(&req.Template, "template", "", "a golden template to restore from")
	f.StringVar(&req.Checkpoint, "checkpoint", "", "restore this checkpoint into the new machine")
	f.IntVar(&req.VCPUs, "vcpus", 0, "vCPUs")
	f.IntVar(&req.MemMiB, "mem-mib", 0, "memory in MiB")
	f.StringVar(&req.App, "app", "", "the app this machine belongs to; machines in one app reach each other by name")
	f.StringVar(&req.Cmd, "cmd", "", "the start command, overriding the image")
	f.StringArrayVar(&envPairs, "env", nil, "an environment variable, KEY=value (repeatable)")
	f.StringVar(&req.Volume, "volume", "", "attach this volume")
	f.BoolVar(&skipConsole, "skip-console", false, "exit after creating instead of opening a console")
	Describe(c, Doc{
		What: "A create is a restore from a golden template, not a boot, which is\n" +
			"why it is instant. The machine gets a stable URL derived from its\n" +
			"name, and that URL is permanent.",
		How: "With no name one is generated. With no image the golden template is\n" +
			"used, which carries a shell, git, node and python. When stdout is a\n" +
			"terminal a console opens on the new machine straight away; pass\n" +
			"--skip-console or pipe the output to skip that.",
		Examples: []string{
			"pilot machines create",
			"pilot machines create scratch --skip-console",
			"pilot machines create api --image 9209a9fe-... --env PORT=8080",
			"# a machine from a checkpoint",
			"pilot machines create --checkpoint ck-3a3077f4",
		},
		Related: []string{
			"pilot console   open a shell on an existing machine",
			"pilot deploy    build and run a directory as a service instead",
		},
	})
	return c
}

func newMachinesInfoCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "info <machine>",
		Short: "everything about one machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(m)
			}
			rows := [][]string{
				{"NAME", m.Name},
				{"ID", m.ID},
				{"STATE", m.State},
				{"URL", m.URL},
				{"HOST", m.HostID},
				{"SIZE", fmt.Sprintf("%d vCPU, %d MiB", m.VCPUs, m.MemMiB)},
				{"CREATED", unixTime(m.CreatedAt)},
				{"LAST ACTIVITY", unixTime(m.LastActivity)},
			}
			if m.App != "" {
				rows = append(rows, []string{"APP", m.App})
			}
			if m.ImageRef != "" {
				rows = append(rows, []string{"IMAGE", m.ImageRef})
			}
			if m.VolumeID != "" {
				rows = append(rows, []string{"VOLUME", m.VolumeID})
			}
			if m.ServiceID != "" {
				rows = append(rows, []string{"SERVICE", m.ServiceID})
				rows = append(rows, []string{"RELEASE", m.ReleaseID})
			}
			if m.CustomDomain != "" {
				rows = append(rows, []string{"DOMAIN", m.CustomDomain})
			}
			rows = append(rows, []string{"AUTO STOP", m.Knobs.AutoStop}, []string{"AUTO START", strconv.FormatBool(m.Knobs.AutoStart)})
			return env.W.Table([]string{"", ""}, rows)
		},
	}
	Describe(c, Doc{
		Examples: []string{
			"pilot machines info scratch",
			"pilot machines info m-687ff8f4 --json",
		},
	})
	return c
}

func newMachinesDestroyCmd(env *Env) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:     "destroy <machine>",
		Aliases: []string{"rm", "delete"},
		Short:   "destroy a machine",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if !force {
				if err := confirm(env, fmt.Sprintf("destroy %s (%s)? Its disk and its checkpoints are deleted and its URL is released.", m.Name, m.ID)); err != nil {
					return err
				}
			}
			if err := client.Machines.Destroy(c.Context(), m.ID); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"destroyed": m.ID})
			}
			env.W.Linef("destroyed %s", m.ID)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the confirmation")
	Describe(c, Doc{
		Warning: "This is irreversible. Destroying a machine permanently deletes:\n" +
			"  - its disk, and every file on it\n" +
			"  - every checkpoint taken of it\n" +
			"  - its URL, which is released for reuse\n\n" +
			"A confirmation prompt asks first. Pass --force or -y to skip it.\n" +
			"A volume attached to the machine is NOT deleted; it outlives it.",
		Examples: []string{
			"pilot machines destroy scratch",
			"pilot machines destroy scratch --force",
			"# for a script",
			"pilot machines destroy m-687ff8f4 -y",
		},
		Related: []string{
			"pilot machines suspend   keep it, stop paying for CPU and memory",
			"pilot machines checkpoint   save its state first",
		},
	})
	return c
}

// newMachinesLifecycleCmd builds one of the four state changes, which differ
// only in the verb and the SDK call.
func newMachinesLifecycleCmd(env *Env, verb, short, method, how string) *cobra.Command {
	c := &cobra.Command{
		Use:   verb + " <machine>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			ctx := c.Context()
			switch method {
			case "Start":
				err = client.Machines.Start(ctx, m.ID)
			case "Stop":
				err = client.Machines.Stop(ctx, m.ID)
			case "Suspend":
				err = client.Machines.Suspend(ctx, m.ID)
			case "Wake":
				err = client.Machines.Wake(ctx, m.ID)
			}
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{verb: m.ID})
			}
			env.W.Linef("%s %s", verb, m.ID)
			return nil
		},
	}
	Describe(c, Doc{
		How:      how,
		Examples: []string{"pilot machines " + verb + " scratch"},
	})
	return c
}

func newMachinesExecCmd(env *Env) *cobra.Command {
	var (
		dir      string
		envPairs []string
		tty      bool
		noStdin  bool
	)
	c := &cobra.Command{
		Use:     "exec <machine> -- <command> [args...]",
		Aliases: []string{"x"},
		Short:   "run a command on a machine and get its output",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			vars, err := parseEnv(envPairs)
			if err != nil {
				return err
			}
			if tty {
				return runConsole(c, env, client, m.ID, args[1:])
			}
			return runExec(c, env, client, m.ID, args[1:], pilots.ExecStreamOptions{
				Dir: dir, Env: vars, Stdin: !noStdin,
			})
		},
	}
	f := c.Flags()
	f.StringVar(&dir, "dir", "", "working directory for the command")
	f.StringArrayVar(&envPairs, "env", nil, "an environment variable, KEY=value (repeatable)")
	f.BoolVar(&tty, "tty", false, "allocate a pseudo-terminal; the same as `pilot console`")
	f.BoolVar(&noStdin, "no-stdin", false, "do not forward stdin")
	Describe(c, Doc{
		When: "For one command whose output you want back: a test run, a file\n" +
			"listing, a build. For a shell to work in, `pilot console`.",
		How: "stdin is forwarded unless --no-stdin, so `cat file | pilot x m -- tee\n" +
			"/tmp/f` works. The command's exit status becomes this command's exit\n" +
			"status, so a failing test fails the pipeline.",
		Examples: []string{
			"pilot machines exec scratch -- ls -la",
			"pilot x scratch -- npm test",
			"pilot x scratch --dir /app --env CI=1 -- make",
			"# output only, nothing on stdin",
			"pilot x scratch --no-stdin -- sh -c 'run-output-only-job'",
		},
		Related: []string{
			"pilot console   an interactive shell",
		},
	})
	return c
}

// runExec streams a non-interactive command: stdin in, stdout and stderr
// out, and the remote exit status as this process's.
func runExec(c *cobra.Command, env *Env, client *pilots.Client, id string, argv []string, opts pilots.ExecStreamOptions) error {
	stream, err := client.Machines.ExecStream(c.Context(), id, argv, opts)
	if err != nil {
		return err
	}
	defer stream.Close()

	if opts.Stdin && stream.Stdin != nil {
		go func() {
			_, _ = io.Copy(stream.Stdin, os.Stdin)
			_ = stream.Stdin.Close()
		}()
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(os.Stdout, stream.Stdout); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stderr, stream.Stderr); done <- struct{}{} }()

	code, err := stream.Wait()
	<-done
	<-done
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

func newMachinesLogsCmd(env *Env) *cobra.Command {
	var follow bool
	c := &cobra.Command{
		Use:   "logs <machine>",
		Short: "a machine's console output",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if !follow {
				text, err := client.Machines.Logs(c.Context(), m.ID)
				if err != nil {
					return err
				}
				_, err = io.WriteString(os.Stdout, text)
				return err
			}
			lines, err := client.Machines.FollowLogs(c.Context(), m.ID)
			if err != nil {
				return err
			}
			for line, err := range lines {
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stdout, line)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until interrupted")
	Describe(c, Doc{
		When: "When a machine is not answering, or a deploy's replica failed its\n" +
			"health gate: the guest's console says why.",
		Examples: []string{
			"pilot machines logs scratch",
			"pilot machines logs scratch -f",
		},
	})
	return c
}

func newMachinesCheckpointCmd(env *Env) *cobra.Command {
	var comment string
	c := &cobra.Command{
		Use:   "checkpoint <machine>",
		Short: "save a point-in-time snapshot of a machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			cp, err := client.Machines.Checkpoint(c.Context(), m.ID, comment)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(cp)
			}
			return env.W.Table([]string{"ID", "SEQ", "DURABLE"}, [][]string{{cp.ID, strconv.Itoa(cp.Seq), strconv.FormatBool(cp.Durable)}})
		},
	}
	c.Flags().StringVar(&comment, "comment", "", "a note to find it by later")
	Describe(c, Doc{
		What: "A checkpoint captures the machine's memory and disk at one moment.\n" +
			"The machine keeps running. Later, `restore` brings the checkpoint\n" +
			"back as a new machine, or `create --checkpoint` clones it.",
		When: "Before something risky. Before handing a sandbox to an agent. When a\n" +
			"state took a long time to build and you want to start there again.",
		How: "Taking one is instant; DURABLE turns true once the upload to object\n" +
			"storage lands, and a restore waits for that.",
		Examples: []string{
			"pilot machines checkpoint scratch --comment 'deps installed'",
			"pilot machines checkpoints scratch",
		},
		Related: []string{
			"pilot machines restore      bring one back",
			"pilot machines checkpoints  list them",
		},
	})
	return c
}

func newMachinesCheckpointsCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "checkpoints <machine>",
		Short: "list a machine's checkpoints",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := resolveMachine(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			list, err := client.Machines.ListCheckpoints(c.Context(), m.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				if list == nil {
					list = []pilots.Checkpoint{}
				}
				return env.W.JSONValue(list)
			}
			if len(list) == 0 {
				env.W.Notef("no checkpoints of %s", m.Name)
				return nil
			}
			rows := make([][]string, 0, len(list))
			for _, cp := range list {
				rows = append(rows, []string{cp.ID, strconv.Itoa(cp.Seq), strconv.FormatBool(cp.Durable), unixTime(cp.CreatedAt), cp.Comment})
			}
			return env.W.Table([]string{"ID", "SEQ", "DURABLE", "CREATED", "COMMENT"}, rows)
		},
	}
	Describe(c, Doc{Examples: []string{"pilot machines checkpoints scratch"}})
	return c
}

func newMachinesRestoreCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "restore <checkpoint-id>",
		Short: "bring a checkpoint back as a new machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := client.Checkpoints.Restore(c.Context(), args[0])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(m)
			}
			return env.W.Table(machineHeaders, [][]string{machineRow(m)})
		},
	}
	Describe(c, Doc{
		How: "The restored machine is a NEW machine with its own name and URL; the\n" +
			"one the checkpoint was taken from is untouched. Nothing is\n" +
			"overwritten, so there is nothing to lose here.",
		Examples: []string{
			"pilot machines restore ck-3a3077f4",
		},
		Related: []string{
			"pilot machines create --checkpoint <id>   the same, with a name of your choosing",
		},
	})
	return c
}
