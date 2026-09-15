package cli

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

// What a database engine says about itself.
//
// The platform's own metrics are about the MACHINE: CPU, memory, requests,
// restores. None of them can answer "is this database about to run out of
// connections", because that number exists only inside the engine. Reading it
// means asking the engine, and every engine has its own client already inside
// its own image.

func newMetricsCmd(env *Env) *cobra.Command {
	var raw bool
	c := &cobra.Command{
		Use:   "metrics <service>",
		Short: "what a database engine says about itself",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			svc, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			engine := svc.Labels["pilot.engine"]
			if engine == "" {
				return out.Failf("`pilot add postgres` adds one that is labelled",
					"%s is not a database; there is no engine to ask", svc.Name)
			}
			cmd := pilots.MetricsCommand(engine)
			if cmd == "" {
				return out.Failf("this reads postgres, mysql, redis and mongo",
					"no metrics command for engine %q", engine)
			}

			m, err := engineReplica(c.Context(), client, svc.ID)
			if err != nil {
				return err
			}
			res, err := client.Machines.Exec(c.Context(), m.ID, pilots.ExecRequest{
				Cmd: cmd, User: "root",
			})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return out.Failf("check the database is up: pilot logs "+svc.Name,
					"the engine refused the query (exit %d): %s", res.ExitCode, res.Stderr)
			}

			got := pilots.ParseEngineMetrics(engine, res.Stdout)
			if raw {
				env.W.Linef("%s", got.Raw)
				return nil
			}
			if env.W.JSON {
				return env.W.JSONValue(got)
			}

			rows := [][]string{
				{"ENGINE", engine},
				{"CONNECTIONS", connectionsLine(got)},
			}
			if got.CacheHitRatio > 0 {
				rows = append(rows, []string{"CACHE HITS", fmt.Sprintf("%.2f%%", got.CacheHitRatio*100)})
			}
			if got.Commits > 0 || got.Rollbacks > 0 {
				rows = append(rows, []string{"COMMITS", strconv.FormatInt(got.Commits, 10)})
				rows = append(rows, []string{"ROLLBACKS", strconv.FormatInt(got.Rollbacks, 10)})
			}
			if got.DataBytes > 0 {
				rows = append(rows, []string{"DATA", humanBytes(got.DataBytes)})
			}
			if got.UptimeSeconds > 0 {
				rows = append(rows, []string{"ENGINE UPTIME", humanDuration(got.UptimeSeconds)})
			}
			return env.W.Table([]string{"", ""}, rows)
		},
	}
	c.Flags().BoolVar(&raw, "raw", false, "the engine's own output, unparsed")
	Describe(c, Doc{
		What: "The numbers only the engine knows: connections against its limit, how\n" +
			"much it is serving from memory, commits against rollbacks.",
		When: "When a database is slow or refusing work and the machine's own CPU and\n" +
			"memory look fine, which is most of the time.",
		How: "Read by running the engine's own client inside the machine, so there is\n" +
			"no exporter to install and nothing extra running beside your database.\n\n" +
			"CONNECTIONS against the limit is the one to watch: a Postgres spends a\n" +
			"process per connection, and an application that opens one per request\n" +
			"exhausts it long before it exhausts the machine.\n\n" +
			"ENGINE UPTIME is the engine's, not the machine's. A database that\n" +
			"restarted an hour ago inside a machine that has been up a week is a\n" +
			"database that crashed.",
		Examples: []string{
			"pilot metrics postgres",
			"pilot metrics postgres --json",
			"pilot metrics postgres --raw",
		},
		Related: []string{
			"pilot logs      what the engine printed",
			"pilot console   a shell beside it",
		},
	})
	return c
}

// connectionsLine renders the pair, and says when it is close to the ceiling.
//
// The warning is part of the number: "42" means nothing, "42 / 100" means
// something, and "95 / 100" means do something now.
func connectionsLine(m pilots.EngineMetrics) string {
	if m.MaxConnections <= 0 {
		return strconv.Itoa(m.Connections)
	}
	line := fmt.Sprintf("%d / %d", m.Connections, m.MaxConnections)
	if float64(m.Connections) >= 0.8*float64(m.MaxConnections) {
		line += "  (near the limit; consider a pooler)"
	}
	return line
}

// engineReplica is a running machine of the service, to ask.
//
// Any of them: a database service runs one replica, and where it runs more the
// engine's own figures are per process anyway.
func engineReplica(ctx context.Context, client *pilots.Client, serviceID string) (*pilots.Machine, error) {
	machines, err := client.Machines.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range machines {
		m := &machines[i]
		if m.ServiceID == serviceID && m.State == "running" {
			return m, nil
		}
	}
	return nil, out.Failf("start it, or check pilot status",
		"no running replica to ask")
}

// humanBytes renders a size the way an operator reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanDuration renders seconds as the largest unit that stays readable.
func humanDuration(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	default:
		return fmt.Sprintf("%dd", sec/86400)
	}
}

// newDBCmd groups the database operations that are not ordinary machine
// operations. Recovery is the only one so far, and it is here rather than under
// `volumes` because what a person wants back is a database, not a disk.
func newDBCmd(env *Env, getenv config.Env) *cobra.Command {
	c := &cobra.Command{
		Use:     "db",
		Aliases: []string{"database"},
		Short:   "database operations: recovery, above all",
	}
	Describe(c, Doc{
		What: "What a database needs beyond what a machine needs. Chiefly getting\n" +
			"back to a moment before something went wrong.",
		Related: []string{
			"pilot db connect  a shell on it, in the engine's own client",
			"pilot add       add a database to a project",
			"pilot metrics   what the engine says about itself",
		},
	})
	c.AddCommand(newDBConnectCmd(env, getenv))
	c.AddCommand(newDBHACmd(env))
	c.AddCommand(newDBRestoreCmd(env))
	return c
}

func newDBRestoreCmd(env *Env) *cobra.Command {
	var (
		to     string
		latest bool
		name   string
	)
	c := &cobra.Command{
		Use:   "restore <service>",
		Short: "bring a Postgres back to a moment in the past",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if to == "" && !latest {
				return out.Failf("pass --to 2026-09-12T10:15:00Z, or --latest",
					"name the moment to recover to")
			}
			if to != "" && latest {
				return out.Failf("pass one of them", "--to and --latest contradict")
			}
			target := to
			if latest {
				target = "latest"
			} else if _, err := time.Parse(time.RFC3339, to); err != nil {
				return out.Failf("write it as 2026-09-12T10:15:00Z",
					"--to %q is not an RFC 3339 time", to)
			}

			client, err := env.Client()
			if err != nil {
				return err
			}
			svc, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if svc.Labels["pilot.engine"] != "postgres" {
				return out.Failf("`pilot volumes snapshots restore` puts a volume back to a snapshot",
					"%s is not a Postgres; point-in-time recovery needs an archived write-ahead log", svc.Name)
			}
			if svc.VolumeID == "" {
				return out.Failf("check `pilot services info "+svc.Name+"`",
					"%s has no archive volume to recover from", svc.Name)
			}

			// A NEW service, beside the old one, which is the whole point: a
			// recovery you cannot compare against the original is a recovery
			// you have to trust. The old database keeps serving throughout.
			restoreName := name
			if restoreName == "" {
				restoreName = svc.Name + "-restore"
			}

			// The archive is FORKED rather than shared. Two Postgres processes
			// writing one archive volume is a corrupted archive, and it would
			// corrupt the one belonging to the database still serving.
			env.W.Notef("snapshotting %s's archive", svc.Name)
			snap, err := client.Volumes.Snapshot(c.Context(), svc.VolumeID)
			if err != nil {
				return err
			}
			env.W.Notef("forking the archive; this copies data, so it is not instant")
			fork, err := client.Volumes.ForkSnapshot(c.Context(), svc.VolumeID,
				snap.Snapshot, restoreName+"-archive")
			if err != nil {
				return err
			}

			// Created private, with the recovery target in its environment.
			// The entrypoint reads it, untars the newest base at or before that
			// moment, replays the archive and promotes -- and pg_isready passes
			// only after the promotion, so the HEALTH GATE is the restore gate:
			// a recovery that cannot reach its target never becomes a release.
			restored, err := client.Services.Create(c.Context(), pilots.CreateServiceRequest{
				Name: restoreName, App: svc.App, Release: svc.ReleaseID,
				Replicas: 1, Volume: fork.ID, Private: true,
				Env:    map[string]string{"PILOT_PG_RESTORE_TARGET": target},
				Labels: map[string]string{"pilot.engine": "postgres"},
			})
			if err != nil {
				return err
			}

			if env.W.JSON {
				return env.W.JSONValue(map[string]any{
					"service": restored, "archive_snapshot": snap.Snapshot, "volume": fork.ID,
				})
			}
			env.W.Linef("recovering %s to %s as %s", svc.Name, target, restored.Name)
			env.W.Notef("it is reachable at %s.internal once the recovery reaches its "+
				"target; %s is untouched and still serving", restored.Name, svc.Name)
			env.W.Notef("compare the two, then point your application at whichever is right")
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&to, "to", "", "the moment to recover to, RFC 3339 (2026-09-12T10:15:00Z)")
	f.BoolVar(&latest, "latest", false, "recover as far forward as the archive goes")
	f.StringVar(&name, "name", "", "name the recovered service; defaults to <service>-restore")
	Describe(c, Doc{
		What: "Point-in-time recovery: a base backup, plus every write-ahead log\n" +
			"segment archived after it, replayed up to the moment you name.",
		When: "After a bad migration, a wrong DELETE, or anything else where the\n" +
			"problem is that the data is now correct-looking and wrong.",
		How: "The recovery runs as a NEW service beside the old one, on a FORK of the\n" +
			"archive. The original keeps serving, and you can query both and compare\n" +
			"before deciding anything.\n\n" +
			"The archive is forked rather than shared because two Postgres processes\n" +
			"writing one archive is a corrupted archive -- including the one\n" +
			"belonging to the database still serving your application.\n\n" +
			"Postgres only, and only in the default wal-archive mode. A\n" +
			"durable-volume database recovers through `pilot volumes snapshots\n" +
			"restore`, which goes back to a snapshot rather than to an arbitrary\n" +
			"moment.",
		Warning: "How far back you can go is bounded by the oldest base backup the\n" +
			"archive still holds, which is four weeks by default.",
		Examples: []string{
			"pilot db restore postgres --to 2026-09-12T10:15:00Z",
			"pilot db restore postgres --latest",
			"pilot db restore postgres --to 2026-09-12T10:15:00Z --name before-migration",
		},
		Related: []string{
			"pilot volumes snapshots   what the archive volume has",
			"docs/honesty.md           how long each kind of recovery takes",
		},
	})
	return c
}
