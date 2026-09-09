package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// contextFile is the directory-local marker `pilot use` writes, the way
// `sprite use` writes `.sprite` and `nvm use` writes `.nvmrc`: a machine name
// or id on one line. Every command that takes a machine reads it when none is
// given, walking up from the working directory, so a project's sandbox is
// named once and then never typed again.
const contextFile = ".pilot"

// contextMachine finds the nearest .pilot above the working directory.
func contextMachine() (name, path string) {
	dir, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	for {
		p := filepath.Join(dir, contextFile)
		if raw, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				return v, p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// machineArg is the machine a command should act on: the argument if one was
// given, else a picker under -s/--select, else the .pilot context, else a
// refusal that says how to name one. The order matters: an explicit argument
// always wins, and --select is an explicit request to choose.
func machineArg(c *cobra.Command, env *Env, client *pilots.Client, arg string) (*pilots.Machine, error) {
	if arg != "" {
		return resolveMachine(c.Context(), client, arg)
	}
	if env.Select {
		return pickMachine(c, env, client)
	}
	if name, path := contextMachine(); name != "" {
		m, err := resolveMachine(c.Context(), client, name)
		if err != nil {
			return nil, out.Failf(fmt.Sprintf("%s names %s; run `pilot use` to pick another or `pilot use --unset`", path, name), "%v", err)
		}
		return m, nil
	}
	return nil, out.Failf("name a machine, pass -s to pick one, or run `pilot use <machine>` in this directory", "no machine given and no .pilot context here")
}

// splitAtDash separates a machine argument from a command that follows `--`.
// `pilot x scratch -- npm test` names the machine; `pilot x -- npm test`
// does not, and takes it from --select or the .pilot context. cobra strips
// the `--` and reports where it was, which is how the two are told apart.
func splitAtDash(c *cobra.Command, args []string) (machine string, argv []string) {
	dash := c.ArgsLenAtDash()
	switch {
	case dash == 0:
		return "", args
	case dash > 0:
		return args[0], args[dash:]
	case len(args) > 0:
		return args[0], args[1:]
	}
	return "", nil
}

func newUseCmd(env *Env) *cobra.Command {
	var unset bool
	c := &cobra.Command{
		Use:   "use [machine]",
		Short: "make a machine the default for this directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if unset {
				if err := os.Remove(contextFile); err != nil && !os.IsNotExist(err) {
					return err
				}
				if env.W.JSON {
					return env.W.JSONValue(map[string]bool{"unset": true})
				}
				env.W.Notef("removed ./%s", contextFile)
				return nil
			}
			client, err := env.Client()
			if err != nil {
				return err
			}
			var m *pilots.Machine
			if len(args) == 1 {
				if m, err = resolveMachine(c.Context(), client, args[0]); err != nil {
					return err
				}
			} else {
				if m, err = pickMachine(c, env, client); err != nil {
					return err
				}
			}
			if err := os.WriteFile(contextFile, []byte(m.Name+"\n"), 0o644); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"machine": m.Name, "id": m.ID, "path": contextFile})
			}
			env.W.Notef("using %s (%s) in this directory", m.Name, m.ID)
			return nil
		},
	}
	c.Flags().BoolVar(&unset, "unset", false, "remove the .pilot file from this directory")
	Describe(c, Doc{
		What: "Writes a .pilot file naming a machine. Every command that takes a\n" +
			"machine reads it when none is given, walking up from the working\n" +
			"directory, so `pilot c`, `pilot x -- npm test` and `pilot logs` all\n" +
			"mean this machine from now on. Like `nvm use` or `asdf local`.",
		How: "With no name, an interactive list is offered. Commit the file if the\n" +
			"whole team shares the machine; ignore it if the machine is yours.",
		Examples: []string{
			"pilot use scratch",
			"pilot use              # pick from a list",
			"pilot use --unset",
		},
		Related: []string{
			"pilot machines destroy   removes a .pilot that points at the destroyed machine",
		},
	})
	return c
}

// pickMachine is the -s/--select experience: a numbered list on stderr and
// one keystroke of an answer, so an id is never typed by hand.
func pickMachine(c *cobra.Command, env *Env, client *pilots.Client) (*pilots.Machine, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, out.Failf("name the machine as an argument", "there is no terminal to pick from")
	}
	machines, err := client.Machines.List(c.Context())
	if err != nil {
		return nil, err
	}
	if len(machines) == 0 {
		return nil, out.Failf("pilot machines create makes one", "no machines in %s", orgLabel(env))
	}
	width := 0
	for _, m := range machines {
		if len(m.Name) > width {
			width = len(m.Name)
		}
	}
	for i, m := range machines {
		fmt.Fprintf(os.Stderr, "%3d  %-*s  %-9s  %s\n", i+1, width, m.Name, m.State, m.ID)
	}
	fmt.Fprintf(os.Stderr, "machine [1-%d]: ", len(machines))
	answer, err := readLine()
	if err != nil {
		return nil, out.Failf("name the machine as an argument", "no choice made")
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(machines) {
		return nil, out.Failf(fmt.Sprintf("answer a number from 1 to %d", len(machines)), "%q is not a choice", answer)
	}
	return &machines[n-1], nil
}
