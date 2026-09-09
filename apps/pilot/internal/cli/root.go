package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

// Env carries what every command needs: the resolved fleet, the writer, and a
// lazily-built client. It is passed explicitly rather than held in a package
// variable so a test can drive a command against its own fleet and buffers.
type Env struct {
	W *out.Writer

	APIURL config.Resolved
	APIKey config.Resolved
	Org    config.Resolved

	// Yes answers every confirmation, for scripts and agents.
	Yes bool
	// Select asks for the machine from a list instead of an argument.
	Select bool

	client *pilots.Client
}

// Client returns the SDK client, refusing early and helpfully when there is no
// key. The refusal names both ways to supply one, because an operator hitting
// this has either never logged in or is in a shell that lost the environment.
func (e *Env) Client() (*pilots.Client, error) {
	if e.client != nil {
		return e.client, nil
	}
	if e.APIKey.Value == "" {
		return nil, out.Failf(
			"run pilot login, or set PILOT_API_KEY",
			"no API key for %s", e.APIURL.Value)
	}
	opts := []pilots.Option{pilots.WithBaseURL(e.APIURL.Value)}
	if e.Org.Value != "" {
		opts = append(opts, pilots.WithOrg(e.Org.Value))
	}
	e.client = pilots.New(e.APIKey.Value, opts...)
	return e.client, nil
}

// NewRoot builds the command tree.
func NewRoot(getenv config.Env) *cobra.Command {
	var (
		flagAPIURL string
		flagAPIKey string
		flagOrg    string
		flagJSON   bool
		flagYes    bool
		flagSelect bool
	)
	env := &Env{}

	root := &cobra.Command{
		Use:           "pilot",
		Short:         "sandboxes and services on one primitive",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			url, key, org, err := config.Resolve(getenv, flagAPIURL, flagAPIKey, flagOrg)
			if err != nil {
				return err
			}
			env.APIURL, env.APIKey, env.Org = url, key, org
			env.Yes = flagYes
			env.Select = flagSelect
			env.W = out.New(flagJSON)
			return nil
		},
	}

	// Persistent, so they are accepted in any position: `pilot -o acme
	// machines ls` and `pilot machines ls -o acme` are the same command. sprite
	// takes its -o and -s anywhere and says so in its examples; the TS CLI
	// accepts them only after the subcommand, which is a papercut every time
	// somebody edits a line they already typed.
	pf := root.PersistentFlags()
	pf.StringVar(&flagAPIURL, "api-url", "", "the fleet to talk to; wins over PILOT_API_URL and the credentials file")
	pf.StringVar(&flagAPIKey, "api-key", "", "the key to authenticate with; wins over PILOT_API_KEY")
	pf.StringVarP(&flagOrg, "org", "o", "", "act as this organization")
	pf.BoolVar(&flagJSON, "json", false, "print the answer as JSON on stdout, errors on stderr")
	pf.BoolVarP(&flagYes, "yes", "y", false, "answer yes to every confirmation; for scripts and agents")
	pf.BoolVarP(&flagSelect, "select", "s", false, "pick the machine from a list instead of naming it")

	Describe(root, Doc{
		What: "pilots runs sandboxes and production services on ONE primitive. A\n" +
			"sandbox and a service are the same machine with different lifecycle\n" +
			"knobs, so there is one vocabulary here, not two: you create a machine,\n" +
			"and `pilot promote` turns it into a service without changing its URL.",
	})

	root.AddCommand(
		InGroup(newMachinesCmd(env), "sandbox"),
		InGroup(newConsoleCmd(env), "sandbox"),
		InGroup(newExecCmd(env), "sandbox"),
		InGroup(newDeployCmd(env, getenv), "deploy"),
		InGroup(newServicesCmd(env), "deploy"),
		InGroup(newPromoteCmd(env), "deploy"),
		InGroup(newStatusCmd(env), "inspect"),
		InGroup(newLogsCmd(env), "inspect"),
		InGroup(newVolumesCmd(env), "storage"),
		InGroup(newDomainsCmd(env), "storage"),
		InGroup(newSecretsCmd(env, getenv), "storage"),
		InGroup(newLoginCmd(env, getenv), "account"),
		InGroup(newLogoutCmd(env, getenv), "account"),
		InGroup(newWhoamiCmd(env), "account"),
		InGroup(newUseCmd(env), "account"),
		InGroup(newAPICmd(env), "help"),
		InGroup(newDoctorCmd(env, getenv), "help"),
		InGroup(newUpgradeCmd(env), "help"),
		InGroup(newVersionCmd(env), "help"),
	)

	UseHelp(root)
	return root
}

// Execute runs the tree and returns a process exit code.
func Execute(ctx context.Context, getenv config.Env, args []string) int {
	root := NewRoot(getenv)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	// A remote command's own status is not a CLI failure: the program that
	// ran has already said whatever it had to say, so nothing is printed.
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return 130 // 128 + SIGINT
	}
	w := out.New(false)
	w.WriteError(err)
	return 1
}

func newWhoamiCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "whoami",
		Short: "which key, fleet and org every other command uses, and where each came from",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			who, err := client.Whoami(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(who)
			}
			return env.W.Table(
				[]string{"", "", "FROM"},
				[][]string{
					{"ORG", who.OrgID, "fleet"},
					{"FLEET", env.APIURL.Value, string(env.APIURL.Source)},
					{"KEY", redact(env.APIKey.Value), string(env.APIKey.Source)},
					{"SCOPES", joinScopes(who.Scopes), "fleet"},
				})
		},
	}
	Describe(c, Doc{
		What: "The answer to \"which fleet am I about to change, and as whom\".",
		When: "Before anything destructive, and first when a command talks to the\n" +
			"wrong place. Every row says where the value came from, so a key that\n" +
			"is not the one you meant is traced to the flag, the environment\n" +
			"variable or the file that supplied it.",
		Examples: []string{
			"pilot whoami",
			"pilot whoami --json",
			"# check a different fleet without changing anything",
			"pilot whoami --api-url http://api.example:8080",
		},
		Related: []string{
			"pilot login    authenticate and store a key",
			"pilot doctor   check the whole local setup, not just the key",
		},
	})
	return c
}

func redact(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12] + "..."
}

func joinScopes(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	out := s[0]
	for _, x := range s[1:] {
		out += ", " + x
	}
	return out
}

func orgLabel(env *Env) string {
	if env.Org.Value != "" {
		return fmt.Sprintf("org %s", env.Org.Value)
	}
	return "this org"
}
