package cli

import (
	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

// Which workspace commands act as.
//
// # Why this is not a team list
//
// The fleet knows an org only as a string on a row. Who the people are, what a
// workspace is called and who belongs to it live in the dashboard's own
// database, which the CLI does not talk to. So `ls` answers what the FLEET can
// answer: the workspaces this key may act as. For an ordinary key that is
// exactly one, and saying so is more useful than an empty list.
//
// # Why switching writes to the credentials file
//
// Because that is where it was always meant to go. `pilot login` has written
// `org_id` there since it existed, and nothing read it back: the stored value
// was dead weight and the only way to persist a workspace was exporting an
// environment variable. Somebody who logged into a team and ran a command
// silently acted as whatever the fleet defaulted them to.

func newOrgsCmd(env *Env, getenv config.Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "orgs",
		Aliases: []string{"org", "workspace", "workspaces"},
		Short:   "which workspace commands act as",
	}
	Describe(c, Doc{
		What: "The workspace every command runs against, and how to change it.",
		How: "A workspace is how the fleet separates one team's machines, services\n" +
			"and volumes from another's. Everything you create belongs to the one\n" +
			"you are acting as, and everything you list is narrowed to it.\n\n" +
			"The order of precedence is --org, then PILOT_ORG, then what `use`\n" +
			"saved. `pilot whoami` shows which one won and where it came from.",
		Related: []string{
			"pilot whoami   the workspace, the scopes and the host answering",
			"pilot login    sign in, which records a workspace to start from",
		},
	})
	c.AddCommand(newOrgsListCmd(env), newOrgsUseCmd(env, getenv))
	return c
}

func newOrgsListCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "the workspaces this key may act as",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			got, err := client.Orgs(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(got)
			}

			rows := make([][]string, 0, len(got.Orgs))
			for _, org := range got.Orgs {
				marker := ""
				if org == got.Current {
					marker = "current"
				}
				rows = append(rows, []string{org, marker})
			}
			if err := env.W.Table([]string{"WORKSPACE", ""}, rows); err != nil {
				return err
			}
			if got.Admin {
				// Said, because an admin key's list is "what exists" rather
				// than "what you may use", and those are different answers to
				// a question that looks the same.
				env.W.Notef("this key is admin-scoped: it may act as any workspace, " +
					"including one not listed here")
			}
			return nil
		},
	}
	Describe(c, Doc{
		What: "The workspaces this key may act as, with the current one marked.",
		How: "Not a list of your teams: the fleet knows a workspace only as an id on\n" +
			"a row, and who belongs to what lives in the dashboard. An ordinary key\n" +
			"acts as exactly one workspace and this says which.",
		Examples: []string{"pilot orgs ls"},
	})
	return c
}

func newOrgsUseCmd(env *Env, getenv config.Env) *cobra.Command {
	var clear bool
	c := &cobra.Command{
		Use:   "use <workspace>",
		Short: "act as this workspace from now on",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			creds, err := config.Load(getenv)
			if err != nil {
				return err
			}
			if creds == nil {
				return out.Failf("`pilot login` first", "there are no saved credentials to record a workspace in")
			}

			if clear || len(args) == 0 {
				if !clear {
					return out.Failf("name one, or pass --clear to stop pinning one",
						"which workspace?")
				}
				creds.OrgID = ""
				if err := config.Save(getenv, creds); err != nil {
					return err
				}
				env.W.Linef("no workspace pinned")
				env.W.Notef("commands now act as whichever workspace the fleet " +
					"associates with this key")
				return nil
			}

			creds.OrgID = args[0]
			if err := config.Save(getenv, creds); err != nil {
				return err
			}
			env.W.Linef("acting as %s", args[0])
			// --org and PILOT_ORG both win over this, so somebody with either
			// set would see no change and have nothing telling them why.
			if env.Org.Value != "" && env.Org.Value != args[0] {
				env.W.Notef("this session is still acting as %s, from %s, which wins "+
					"over the saved workspace", env.Org.Value, env.Org.Source)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&clear, "clear", false, "stop pinning a workspace")
	Describe(c, Doc{
		What: "Record the workspace every later command acts as.",
		How: "Written to the credentials file, beside the key. --org and PILOT_ORG\n" +
			"both win over it, so a script that sets either is unaffected by what\n" +
			"you saved here.",
		Examples: []string{
			"pilot orgs use team-acme",
			"pilot orgs use --clear",
		},
	})
	return c
}
