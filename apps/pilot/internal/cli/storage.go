package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"
)

func newVolumesCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "volumes",
		Aliases: []string{"volume", "vol"},
		Short:   "durable storage that outlives a machine",
	}
	Describe(c, Doc{
		What: "A volume is a disk that is replicated to object storage on every\n" +
			"write, so it survives the machine it is attached to, the host that\n" +
			"machine ran on, and a wipe of that host's local disk. A machine's own\n" +
			"disk is a cache; a volume is the truth.",
		When: "For a database, or anything else whose data must outlive a redeploy.\n" +
			"A stateless app needs none: its disk comes from the release.",
		Related: []string{
			"pilot machines create --volume   attach one at create",
			"pilot machines destroy           does NOT delete an attached volume",
		},
	})
	c.AddCommand(
		newVolumesListCmd(env), newVolumesCreateCmd(env),
		newVolumesSnapshotCmd(env), newVolumesSnapshotsCmd(env), newVolumesRestoreCmd(env),
	)
	return c
}

func newVolumesListCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list volumes",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			vols, err := client.Volumes.List(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				if vols == nil {
					vols = []pilots.Volume{}
				}
				return env.W.JSONValue(vols)
			}
			if len(vols) == 0 {
				env.W.Notef("no volumes in %s", orgLabel(env))
				return nil
			}
			rows := make([][]string, 0, len(vols))
			for _, v := range vols {
				rows = append(rows, []string{v.Name, strconv.Itoa(v.SizeGiB), v.MountPath, orDash(v.MachineID), orDash(v.HostID), v.ID})
			}
			return env.W.Table([]string{"NAME", "GIB", "MOUNT", "MACHINE", "HOST", "ID"}, rows)
		},
	}
	Describe(c, Doc{Examples: []string{"pilot volumes ls"}})
	return c
}

func newVolumesCreateCmd(env *Env) *cobra.Command {
	var req pilots.CreateVolumeRequest
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "create a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			req.Name = args[0]
			v, err := client.Volumes.Create(c.Context(), req)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(v)
			}
			return env.W.Table([]string{"NAME", "GIB", "MOUNT", "ID"}, [][]string{{v.Name, strconv.Itoa(v.SizeGiB), v.MountPath, v.ID}})
		},
	}
	c.Flags().IntVar(&req.SizeGiB, "size-gib", 1, "size in GiB")
	c.Flags().StringVar(&req.MountPath, "mount", "/data", "where the machine sees it")
	Describe(c, Doc{
		Examples: []string{
			"pilot volumes create pgdata --size-gib 10 --mount /var/lib/postgresql",
			"pilot machines create db --volume <id>",
		},
	})
	return c
}

func newDomainsCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "domains",
		Aliases: []string{"domain"},
		Short:   "attach your own hostnames to services",
	}
	Describe(c, Doc{
		What: "Every service already has a platform address. A domain is your own\n" +
			"hostname on top of it: you point a CNAME at the target shown here,\n" +
			"and once it resolves the service answers on both.",
		Related: []string{
			"pilot services info   shows both addresses",
		},
	})
	c.AddCommand(newDomainsListCmd(env), newDomainsAddCmd(env), newDomainsRemoveCmd(env))
	return c
}

func newDomainsListCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list domains",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			domains, err := client.Domains.List(c.Context())
			if err != nil {
				return err
			}
			if env.W.JSON {
				if domains == nil {
					domains = []pilots.DomainResponse{}
				}
				return env.W.JSONValue(domains)
			}
			if len(domains) == 0 {
				env.W.Notef("no domains in %s", orgLabel(env))
				return nil
			}
			rows := make([][]string, 0, len(domains))
			for _, d := range domains {
				rows = append(rows, []string{d.Hostname, d.ServiceID, strconv.FormatBool(d.Verified), d.Target})
			}
			return env.W.Table([]string{"HOSTNAME", "SERVICE", "VERIFIED", "CNAME TARGET"}, rows)
		},
	}
	Describe(c, Doc{Examples: []string{"pilot domains ls"}})
	return c
}

func newDomainsAddCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "add <hostname> <service>",
		Short: "attach a hostname to a service",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			s, err := resolveService(c.Context(), client, args[1])
			if err != nil {
				return err
			}
			d, err := client.Domains.Add(c.Context(), pilots.AddDomainRequest{Hostname: args[0], ServiceID: s.ID})
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(d)
			}
			env.W.Linef("point a CNAME for %s at %s", d.Hostname, d.Target)
			return nil
		},
	}
	Describe(c, Doc{
		How: "The hostname is recorded and a CNAME target is answered. Nothing is\n" +
			"served on it until DNS resolves to that target; `pilot domains ls`\n" +
			"shows VERIFIED once it does.",
		Examples: []string{"pilot domains add shop.example.com web"},
	})
	return c
}

func newDomainsRemoveCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "remove <hostname>",
		Aliases: []string{"rm"},
		Short:   "detach a hostname",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			if err := client.Domains.Remove(c.Context(), args[0]); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"removed": args[0]})
			}
			env.W.Linef("removed %s", args[0])
			return nil
		},
	}
	Describe(c, Doc{
		Warning: "The service stops answering on this hostname immediately. Its\n" +
			"platform address is unaffected.",
		Examples: []string{"pilot domains remove shop.example.com"},
	})
	return c
}
