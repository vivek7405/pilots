package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// urlTarget is whatever a machine or service name resolved to: one of the
// two is set. A machine that is a replica reports its service's mode, which
// is what the router enforces for it.
type urlTarget struct {
	machine *pilots.Machine
	service *pilots.Service
}

func (t urlTarget) name() string {
	if t.service != nil {
		return t.service.Name
	}
	return t.machine.Name
}

func (t urlTarget) url() string {
	if t.service != nil {
		return serviceAddress(t.service)
	}
	return t.machine.URL
}

func (t urlTarget) mode() string {
	if t.service != nil {
		return orPublic(t.service.URLAuth)
	}
	return orPublic(t.machine.URLAuth)
}

func orPublic(mode string) string {
	if mode == "" {
		return pilots.URLAuthPublic
	}
	return mode
}

// resolveURLTarget tries a machine first, then a service, so `pilot url web`
// works whichever kind `web` is. With no argument the .pilot context or
// --select supplies a machine.
func resolveURLTarget(ctx context.Context, c *cobra.Command, env *Env, client *pilots.Client, arg string) (urlTarget, error) {
	if arg == "" {
		m, err := machineArg(c, env, client, "")
		if err != nil {
			return urlTarget{}, err
		}
		return urlTarget{machine: m}, nil
	}
	m, err := resolveMachine(ctx, client, arg)
	if err == nil {
		return urlTarget{machine: m}, nil
	}
	var f *out.Failure
	if !errors.As(err, &f) {
		return urlTarget{}, err
	}
	s, serr := resolveService(ctx, client, arg)
	if serr == nil {
		return urlTarget{service: s}, nil
	}
	return urlTarget{}, out.Failf("pilot machines ls and pilot services ls show what exists", "no machine or service named %s", arg)
}

func newURLCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "url [machine|service]",
		Short: "show a URL and who may reach it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			t, err := resolveURLTarget(c.Context(), c, env, client, first(args))
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"name": t.name(), "url": t.url(), "url_auth": t.mode()})
			}
			return env.W.Table([]string{"", ""}, [][]string{{"NAME", t.name()}, {"URL", t.url()}, {"AUTH", t.mode()}})
		},
	}
	Describe(c, Doc{
		What: "Every machine and service has a permanent URL. Who may reach it is\n" +
			"one of two modes:\n\n" +
			"  public   anyone with the URL (the default)\n" +
			"  org      only a request carrying an API key of the owning org, as\n" +
			"           Authorization: Bearer <key>; anything else is 401, another\n" +
			"           org's key is 403\n\n" +
			"A promoted machine follows its service's mode.",
		When: "public for anything meant to be shared or to take webhooks. org for a\n" +
			"sandbox an agent is working in: it listens on 8080 the moment it\n" +
			"starts, and org keeps that between you and the fleet.",
		Examples: []string{
			"pilot url scratch",
			"pilot url web --json",
			"pilot url update --auth org scratch",
			"# with a .pilot context",
			"pilot url",
		},
		Related: []string{
			"pilot machines create --url-auth org   set it at create",
			"pilot domains                          your own hostname on a service",
		},
	})
	c.AddCommand(newURLUpdateCmd(env))
	return c
}

func newURLUpdateCmd(env *Env) *cobra.Command {
	var auth string
	c := &cobra.Command{
		Use:   "update [machine|service]",
		Short: "change who may reach a URL",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if auth != pilots.URLAuthPublic && auth != pilots.URLAuthOrg {
				return out.Failf("pass --auth public or --auth org", "--auth %q is not a mode", auth)
			}
			client, err := env.Client()
			if err != nil {
				return err
			}
			t, err := resolveURLTarget(c.Context(), c, env, client, first(args))
			if err != nil {
				return err
			}
			mode := auth
			if t.service != nil {
				if _, err := client.Services.Patch(c.Context(), t.service.ID, pilots.UpdateServiceRequest{URLAuth: &mode}); err != nil {
					return err
				}
			} else {
				if _, err := client.Machines.Update(c.Context(), t.machine.ID, pilots.UpdateMachineRequest{URLAuth: &mode}); err != nil {
					return err
				}
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"name": t.name(), "url": t.url(), "url_auth": mode})
			}
			env.W.Linef("%s is now %s", t.url(), mode)
			return nil
		},
	}
	c.Flags().StringVar(&auth, "auth", "", "public or org")
	_ = c.MarkFlagRequired("auth")
	Describe(c, Doc{
		Warning: "--auth public makes the URL reachable by anyone who has it, immediately.\n" +
			"--auth org cuts off every caller without an API key of the org,\n" +
			"including a browser: there is no login page on a workload URL.",
		Examples: []string{
			"pilot url update --auth org scratch",
			"pilot url update --auth public web",
		},
	})
	return c
}
