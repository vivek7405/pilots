package cli

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// What a machine may ask its host for.
//
// # Why this is not `pilot secrets`
//
// `pilot secrets` is the LOCAL store: values on the operator's own disk,
// resolved client-side into a deploy, never sent anywhere as a name a host
// could look up. These are the opposite kind of thing: they live in the fleet,
// sealed, and the machine fetches them itself at run time.
//
// Two different mechanisms with one name would be the worst outcome, so the
// verbs live under the object they apply to: `pilot machines grant`,
// `pilot services grant`.
//
// # Why a grant replaces
//
// Because merging two partial grants produces a permission nobody wrote. The
// command says so before it does it.

func newGrantCmd(env *Env, kind string) *cobra.Command {
	c := &cobra.Command{
		Use: "grant",
		// "a machine" / "a service": the article reads as a typo without it,
		// and this string is the one line most people ever see of this group.
		Short:   "what a " + kind + " may ask its host for",
		Aliases: []string{"grants"},
	}
	Describe(c, Doc{
		What: "A machine holds no API key. It asks the broker running inside its own\n" +
			"network namespace, and a grant is what says the answer may be anything\n" +
			"but no.",
		How: "Deny by default: with no grant, a machine can mint no token and read no\n" +
			"secret. A grant REPLACES rather than merges, because merging two partial\n" +
			"grants produces a permission nobody wrote.\n\n" +
			"You may grant only scopes you already hold, and never admin.",
		Related: []string{
			"pilot secrets    the LOCAL store, resolved into a deploy; a different thing",
			"pilot tokens     API keys, which a person holds and a machine does not",
		},
	})
	c.AddCommand(newGrantSetCmd(env, kind), newGrantShowCmd(env, kind), newGrantClearCmd(env, kind))
	return c
}

func newGrantSetCmd(env *Env, kind string) *cobra.Command {
	var (
		scopes  []string
		secrets []string
	)
	c := &cobra.Command{
		Use:   "set <" + kind + ">",
		Short: "replace what it may ask for",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			values := map[string]string{}
			for _, pair := range secrets {
				name, value, ok := strings.Cut(pair, "=")
				if !ok || name == "" {
					return out.Failf("write it as NAME=value",
						"%q is not a NAME=value pair", pair)
				}
				values[name] = value
			}
			req := pilots.GrantRequest{Scopes: scopes, Secrets: values}

			id, err := resolveGrantTarget(c, env, client, kind, args[0])
			if err != nil {
				return err
			}
			var got *pilots.GrantResponse
			if kind == "machine" {
				got, err = client.Machines.Grant(c.Context(), id, req)
			} else {
				got, err = client.Services.Grant(c.Context(), id, req)
			}
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(got)
			}
			return printGrant(env, got, kind)
		},
	}
	f := c.Flags()
	f.StringSliceVar(&scopes, "scope", nil, "a scope a minted token may carry (machines, deploy)")
	f.StringArrayVar(&secrets, "secret", nil, "NAME=value the machine may fetch; repeatable")
	Describe(c, Doc{
		What: "Replace what this " + kind + " may ask its host's broker for.",
		How: "Both halves replace. Passing no --scope removes every scope; passing no\n" +
			"--secret removes every secret. That is deliberate: a grant you can only\n" +
			"add to is a grant nobody can narrow.\n\n" +
			"Secrets granted this way never enter the machine's environment, so they\n" +
			"are in no snapshot of it and on no disk inside it. The machine fetches\n" +
			"them from its broker when it needs them.",
		Warning: "A token already minted lives out its fifteen minutes. Revoking the\n" +
			"grant stops the next one; to stop one now, revoke the token itself.",
		Examples: []string{
			"pilot machines grant set m-123 --scope machines",
			"pilot machines grant set m-123 --secret STRIPE_KEY=sk_live_x",
			"pilot services grant set web --scope deploy --secret A=1 --secret B=2",
		},
	})
	return c
}

func newGrantShowCmd(env *Env, kind string) *cobra.Command {
	c := &cobra.Command{
		Use:   "show <" + kind + ">",
		Short: "what it may ask for, by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			id, err := resolveGrantTarget(c, env, client, kind, args[0])
			if err != nil {
				return err
			}
			var got *pilots.GrantResponse
			if kind == "machine" {
				got, err = client.Machines.GrantOf(c.Context(), id)
			} else {
				got, err = client.Services.GrantOf(c.Context(), id)
			}
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(got)
			}
			return printGrant(env, got, kind)
		},
	}
	Describe(c, Doc{
		What: "The scopes and the secret NAMES granted. Never the values.",
		How: "There is no route that returns a granted value. What a machine is\n" +
			"holding is a question the machine's own broker answers, to the machine.",
		Examples: []string{"pilot machines grant show m-123"},
	})
	return c
}

func newGrantClearCmd(env *Env, kind string) *cobra.Command {
	c := &cobra.Command{
		Use:   "clear <" + kind + ">",
		Short: "remove the grant",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			id, err := resolveGrantTarget(c, env, client, kind, args[0])
			if err != nil {
				return err
			}
			if kind == "machine" {
				err = client.Machines.RevokeGrant(c.Context(), id)
			} else {
				err = client.Services.RevokeGrant(c.Context(), id)
			}
			if err != nil {
				return err
			}
			env.W.Linef("cleared the grant on %s", args[0])
			env.W.Notef("a token already minted lives out its fifteen minutes; " +
				"revoke the token itself to stop one now")
			return nil
		},
	}
	Describe(c, Doc{
		What:     "Remove the grant, so the next credential this " + kind + " asks for is refused.",
		Examples: []string{"pilot machines grant clear m-123"},
	})
	return c
}

// resolveGrantTarget turns a name into an id, through the same resolver every
// other command uses so a name means the same thing everywhere.
func resolveGrantTarget(c *cobra.Command, env *Env, client *pilots.Client, kind, name string) (string, error) {
	if kind == "machine" {
		m, err := resolveMachine(c.Context(), client, name)
		if err != nil {
			return "", err
		}
		return m.ID, nil
	}
	svc, err := resolveService(c.Context(), client, name)
	if err != nil {
		return "", err
	}
	return svc.ID, nil
}

func printGrant(env *Env, got *pilots.GrantResponse, kind string) error {
	scopes := append([]string(nil), got.Scopes...)
	sort.Strings(scopes)
	rows := [][]string{{"ID", got.ID}, {"KIND", kind}}
	if len(scopes) == 0 {
		// Said rather than left blank. An empty line reads as "not loaded"; the
		// sentence is what tells somebody this is a decision.
		rows = append(rows, []string{"SCOPES", "none; no token may be minted"})
	} else {
		rows = append(rows, []string{"SCOPES", strings.Join(scopes, ", ")})
	}
	if len(got.SecretNames) == 0 {
		rows = append(rows, []string{"SECRETS", "none"})
	} else {
		for _, name := range got.SecretNames {
			rows = append(rows, []string{"SECRET", name})
		}
	}
	return env.W.Table([]string{"", ""}, rows)
}
