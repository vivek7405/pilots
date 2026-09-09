package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

func serviceAddress(s *pilots.Service) string {
	if s.CustomDomain != "" {
		return s.CustomDomain
	}
	return s.URL
}

func newServicesCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "services",
		Aliases: []string{"service", "svc", "s"},
		Short:   "inspect and change services",
	}
	Describe(c, Doc{
		What: "A service is a machine with a production lifecycle: a release to run,\n" +
			"a replica count the fleet reconciles to, a health gate every deploy\n" +
			"must pass, and an address that never changes. `pilot deploy` makes\n" +
			"one from a directory; `pilot promote` makes one from a machine.",
		Related: []string{
			"pilot deploy    build a directory and release it",
			"pilot promote   turn a sandbox into a service, keeping its URL",
			"pilot logs      every replica's output in one stream",
		},
	})
	c.AddCommand(
		newServicesListCmd(env),
		newServicesInfoCmd(env),
		newServicesReleasesCmd(env),
		newServicesRollbackCmd(env),
		newServicesSetCmd(env),
	)
	return c
}

func newServicesListCmd(env *Env) *cobra.Command {
	var app string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list services",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			services, err := client.Services.List(c.Context())
			if err != nil {
				return err
			}
			if app != "" {
				kept := services[:0]
				for _, s := range services {
					if s.App == app {
						kept = append(kept, s)
					}
				}
				services = kept
			}
			if env.W.JSON {
				if services == nil {
					services = []pilots.Service{}
				}
				return env.W.JSONValue(services)
			}
			if len(services) == 0 {
				env.W.Notef("no services in %s", orgLabel(env))
				return nil
			}
			rows := make([][]string, 0, len(services))
			for i := range services {
				s := &services[i]
				rows = append(rows, []string{s.Name, s.App, strconv.Itoa(s.Replicas), serviceAddress(s), s.ID})
			}
			return env.W.Table([]string{"NAME", "APP", "REPLICAS", "URL", "ID"}, rows)
		},
	}
	c.Flags().StringVar(&app, "app", "", "only services in this app")
	Describe(c, Doc{
		Examples: []string{
			"pilot services ls",
			"pilot s ls --app shop",
			"pilot services ls --json | jq -r '.[] | [.name, .url] | @tsv'",
		},
	})
	return c
}

func newServicesInfoCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "info <service>",
		Short: "everything about one service",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			s, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(s)
			}
			rows := [][]string{
				{"NAME", s.Name},
				{"ID", s.ID},
				{"APP", s.App},
				{"URL", serviceAddress(s)},
				{"REPLICAS", strconv.Itoa(s.Replicas)},
				{"RELEASE", orDash(s.ReleaseID)},
				{"CREATED", unixTime(s.CreatedAt)},
			}
			if s.CustomDomain != "" {
				rows = append(rows, []string{"PLATFORM URL", s.URL})
			}
			if s.Health != nil {
				rows = append(rows, []string{"HEALTH", fmt.Sprintf("%s %s", s.Health.Type, s.Health.Path)})
			}
			if len(s.DependsOn) > 0 {
				rows = append(rows, []string{"DEPENDS ON", strings.Join(s.DependsOn, ", ")})
			}
			if s.VolumeID != "" {
				rows = append(rows, []string{"VOLUME", s.VolumeID})
			}
			if s.Repo != "" {
				rows = append(rows, []string{"REPO", s.Repo + "@" + s.Branch}, []string{"AUTODEPLOY", strconv.FormatBool(s.Autodeploy)})
			}
			rows = append(rows,
				[]string{"AUTO STOP", s.Knobs.AutoStop},
				[]string{"MIN RUNNING", strconv.Itoa(s.Knobs.MinMachinesRunning)},
			)
			return env.W.Table([]string{"", ""}, rows)
		},
	}
	Describe(c, Doc{Examples: []string{"pilot services info web", "pilot services info web --json"}})
	return c
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newServicesReleasesCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "releases <service>",
		Short: "a service's releases, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			s, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			releases, err := client.Services.Releases(c.Context(), s.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				if releases == nil {
					releases = []pilots.Release{}
				}
				return env.W.JSONValue(releases)
			}
			if len(releases) == 0 {
				env.W.Notef("%s has no releases yet; `pilot deploy` cuts the first", s.Name)
				return nil
			}
			rows := make([][]string, 0, len(releases))
			for _, r := range releases {
				current := ""
				if r.ID == s.ReleaseID {
					current = "current"
				}
				rows = append(rows, []string{r.ID, strconv.FormatBool(r.Healthy), unixTime(r.CreatedAt), orDash(r.RootfsBuildID), current})
			}
			return env.W.Table([]string{"RELEASE", "HEALTHY", "CREATED", "BUILD", ""}, rows)
		},
	}
	Describe(c, Doc{
		What: "A release is one build the service ran. Every deploy cuts one; a\n" +
			"rollback makes an earlier healthy one current again.",
		Examples: []string{"pilot services releases web"},
		Related:  []string{"pilot services rollback   return to the previous healthy release"},
	})
	return c
}

func newServicesRollbackCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "rollback <service>",
		Short: "return to the previous healthy release",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			s, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			r, err := client.Services.Rollback(c.Context(), s.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(r)
			}
			env.W.Linef("%s is on %s", s.Name, r.ID)
			return nil
		},
	}
	Describe(c, Doc{
		How: "The previous healthy release becomes current; the address does not\n" +
			"change and the replicas are replaced blue/green, so nothing is down\n" +
			"in between. A service with one release has nothing to roll back to,\n" +
			"and says so.",
		Examples: []string{"pilot services rollback web"},
	})
	return c
}

func newServicesSetCmd(env *Env) *cobra.Command {
	var (
		replicas   int
		envPairs   []string
		unsetEnv   []string
		secretEnv  []string
		repo       string
		branch     string
		autodeploy string
		domain     string
	)
	c := &cobra.Command{
		Use:   "set <service>",
		Short: "change replicas, env, secrets, the connected repo or the address",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			s, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			var req pilots.UpdateServiceRequest
			changed := false
			if c.Flags().Changed("replicas") {
				req.Replicas = &replicas
				changed = true
			}
			if len(envPairs) > 0 || len(unsetEnv) > 0 {
				vars, err := parseEnv(envPairs)
				if err != nil {
					return err
				}
				if vars == nil {
					vars = map[string]string{}
				}
				// An unset is an explicit empty value on the wire, which the
				// API clears; that is how the TS CLI does it too.
				for _, k := range unsetEnv {
					vars[k] = ""
				}
				req.Env = vars
				changed = true
			}
			if len(secretEnv) > 0 {
				sec, err := parseEnv(secretEnv)
				if err != nil {
					return err
				}
				req.SecretEnv = sec
				changed = true
			}
			if c.Flags().Changed("repo") {
				req.Repo = &repo
				changed = true
			}
			if c.Flags().Changed("branch") {
				req.Branch = &branch
				changed = true
			}
			if c.Flags().Changed("autodeploy") {
				b, err := strconv.ParseBool(autodeploy)
				if err != nil {
					return out.Failf("pass true or false", "--autodeploy %q is not a boolean", autodeploy)
				}
				req.Autodeploy = &b
				changed = true
			}
			if c.Flags().Changed("domain") {
				req.Domain = &domain
				changed = true
			}
			if !changed {
				return out.Failf("see `pilot services set --help`", "nothing to change")
			}
			updated, err := client.Services.Patch(c.Context(), s.ID, req)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(updated)
			}
			env.W.Linef("updated %s", updated.Name)
			return nil
		},
	}
	f := c.Flags()
	f.IntVar(&replicas, "replicas", 0, "how many replicas the fleet reconciles to")
	f.StringArrayVar(&envPairs, "env", nil, "set an environment variable, KEY=value (repeatable)")
	f.StringArrayVar(&unsetEnv, "unset-env", nil, "remove an environment variable (repeatable)")
	f.StringArrayVar(&secretEnv, "secret-env", nil, "REPLACE the sealed environment with these KEY=value pairs (repeatable)")
	f.StringVar(&repo, "repo", "", "the GitHub repo to deploy from, owner/name")
	f.StringVar(&branch, "branch", "", "the branch to deploy")
	f.StringVar(&autodeploy, "autodeploy", "", "true to deploy on every push to the branch")
	f.StringVar(&domain, "domain", "", "give a service that has no address one; an address is permanent, so this works once")
	Describe(c, Doc{
		How: "Every change is a patch: only what you pass changes. An env change\n" +
			"takes effect on the next deploy. --secret-env replaces the whole\n" +
			"sealed environment rather than merging, because a secret that was\n" +
			"meant to be removed must not linger.",
		Warning: "--domain gives an address to a service that has none, exactly once.\n" +
			"An address is permanent -- it survives every deploy, restore and\n" +
			"promote -- so a service that already has one is refused.",
		Examples: []string{
			"pilot services set web --replicas 3",
			"pilot services set web --env LOG_LEVEL=debug --unset-env DEBUG",
			"pilot services set web --secret-env DATABASE_URL=postgres://...",
			"pilot services set web --repo acme/shop --branch main --autodeploy true",
		},
	})
	return c
}

func newPromoteCmd(env *Env) *cobra.Command {
	var (
		replicas int
		domain   string
	)
	c := &cobra.Command{
		Use:   "promote <machine>",
		Short: "turn a sandbox into a durable service, keeping its URL",
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
			s, err := client.Machines.Promote(c.Context(), m.ID, pilots.PromoteRequest{
				Replicas: replicas, CustomDomain: domain,
			})
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(s)
			}
			return env.W.Table([]string{"SERVICE", "URL", "ID"}, [][]string{{s.Name, serviceAddress(s), s.ID}})
		},
	}
	c.Flags().IntVar(&replicas, "replicas", 1, "replicas for the new service")
	c.Flags().StringVar(&domain, "domain", "", "a custom hostname to attach")
	Describe(c, Doc{
		What: "This is the bridge between the two faces of one primitive. The machine\n" +
			"you built in becomes replica one of a service: same id, same disk,\n" +
			"same URL, same token. Nothing is copied and nothing moves.",
		When: "When the thing in a sandbox turned out to be the thing you want to\n" +
			"run. Elsewhere this is a deploy to a different kind of VM; here it\n" +
			"is a lifecycle change on the machine you already have.",
		Examples: []string{
			"pilot promote scratch",
			"pilot promote scratch --replicas 2 --domain shop.example.com",
		},
		Related: []string{
			"pilot services info   what it became",
			"pilot deploy          the other way to make a service, from a directory",
		},
	})
	return c
}

func newStatusCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "hosts in the fleet and machines by state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			ctx := c.Context()
			hosts, err := client.Hosts.List(ctx)
			if err != nil {
				return err
			}
			machines, err := client.Machines.List(ctx)
			if err != nil {
				return err
			}
			counts := map[string]int{}
			for _, m := range machines {
				counts[m.State]++
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"hosts": hosts, "machines": counts, "total": len(machines)})
			}
			hostRows := make([][]string, 0, len(hosts))
			for _, h := range hosts {
				hostRows = append(hostRows, []string{h.ID, strconv.FormatBool(h.Alive), strconv.Itoa(h.CPUFree), strconv.Itoa(h.MemFreeMiB), orDash(h.CPUVendor)})
			}
			if err := env.W.Table([]string{"HOST", "ALIVE", "CPU FREE", "MEM FREE MIB", "CPU"}, hostRows); err != nil {
				return err
			}
			env.W.Linef("")
			var stateRows [][]string
			for _, st := range []string{"creating", "running", "suspended", "stopped", "error"} {
				stateRows = append(stateRows, []string{st, strconv.Itoa(counts[st])})
			}
			stateRows = append(stateRows, []string{"total", strconv.Itoa(len(machines))})
			return env.W.Table([]string{"STATE", "MACHINES"}, stateRows)
		},
	}
	Describe(c, Doc{
		When: "First, when something is wrong: a host that is not alive explains a\n" +
			"machine that will not wake, and a pile of `error` machines is worth a\n" +
			"look before creating more.",
		Examples: []string{"pilot status", "pilot status --json"},
	})
	return c
}
