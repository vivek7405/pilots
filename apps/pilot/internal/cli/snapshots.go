package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Point-in-time copies of a volume.
//
// The one recovery a volume did not have. Until this existed, the only version
// of a volume's data was its current contents, which is the thing that goes
// wrong.

func newVolumesSnapshotsCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "snapshots <volume>",
		Aliases: []string{"snapshot", "snaps"},
		Short:   "point-in-time copies of a volume",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			res, err := client.Volumes.Snapshots(c.Context(), args[0])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}
			if len(res.Snapshots) == 0 {
				env.W.Notef("%s has no snapshots; `pilot volumes snapshot %s` takes one",
					args[0], args[0])
				return nil
			}
			rows := make([][]string, 0, len(res.Snapshots))
			for _, s := range res.Snapshots {
				rows = append(rows, []string{s})
			}
			return env.W.Table([]string{"SNAPSHOT"}, rows)
		},
	}
	Describe(c, Doc{
		How: "Newest first. A snapshot is named by the moment it was taken, in UTC,\n" +
			"and that name is what `restore` takes.",
		Examples: []string{"pilot volumes snapshots data"},
	})
	return c
}

func newVolumesSnapshotCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "snapshot <volume>",
		Short: "take a point-in-time copy of a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			res, err := client.Volumes.Snapshot(c.Context(), args[0])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}
			env.W.Linef("%s", res.Snapshot)
			return nil
		},
	}
	Describe(c, Doc{
		How: "A clone inside the volume's own filesystem, so no data is copied and it\n" +
			"costs milliseconds however large the volume is. Nothing you are charged\n" +
			"for grows until the live volume overwrites blocks the snapshot still\n" +
			"holds.\n\n" +
			"A machine RUNNING on the volume is paused for the moment the clone is\n" +
			"taken -- milliseconds -- because a copy made while the guest is writing\n" +
			"captures a filesystem mid-update.",
		Examples: []string{
			"pilot volumes snapshot data",
			"pilot volumes snapshot data --json",
		},
		Related: []string{
			"pilot volumes snapshots   list them",
			"pilot volumes restore     put one back",
		},
	})
	return c
}

func newVolumesRestoreCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "restore <volume> <snapshot>",
		Short: "put a snapshot back as the volume's live disk",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			// The root command's own -y skips this. A restore throws away
			// everything written since the snapshot, and nothing anywhere
			// keeps a copy of it.
			if err := confirm(env, fmt.Sprintf(
				"restore %s to %s? everything written since is lost", args[0], args[1])); err != nil {
				return err
			}
			client, err := env.Client()
			if err != nil {
				return err
			}
			res, err := client.Volumes.RestoreSnapshot(c.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}
			env.W.Linef("%s is back at %s", args[0], res.Snapshot)
			return nil
		},
	}
	Describe(c, Doc{
		How: "The snapshot becomes the volume's disk. Everything written since it was\n" +
			"taken is gone: this is a restore, not a merge.\n\n" +
			"A machine RUNNING on the volume is refused, because replacing the disk\n" +
			"under a live guest corrupts it. Suspend or destroy it first. A SUSPENDED\n" +
			"machine is allowed and loses its saved memory, so it cold-boots onto the\n" +
			"restored disk -- waking with memory that cached the OLD filesystem would\n" +
			"corrupt the new one within seconds.",
		Warning: "Everything written after the snapshot is lost, and nothing keeps a copy\n" +
			"of it. Take a snapshot first if the current state might matter.",
		Examples: []string{
			"pilot volumes restore data 20260912T101500Z",
			"pilot -y volumes restore data 20260912T101500Z",
		},
	})
	return c
}
