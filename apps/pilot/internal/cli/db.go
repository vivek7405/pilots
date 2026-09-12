package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

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
