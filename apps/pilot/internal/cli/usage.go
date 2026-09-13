package cli

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// What this org has been charged for, and which machine did the charging.
//
// The second half is the point. An invoice line reading "9,972,448 seconds"
// answers no question anybody has; the question is always "which of my
// machines is that, and why". The ledger's grain has always been per machine,
// so this is a read rather than a new measurement.

func newUsageCmd(env *Env) *cobra.Command {
	var (
		since     string
		until     string
		byMachine bool
	)
	c := &cobra.Command{
		Use:   "usage",
		Short: "what this org has used, and what it cost",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			untilTS, err := parseWhen(until, time.Now())
			if err != nil {
				return out.Failf("pass a duration like 24h, or a date like 2026-09-01",
					"could not read --until: %v", err)
			}
			sinceTS, err := parseWhen(since, time.Unix(untilTS, 0).Add(-24*time.Hour))
			if err != nil {
				return out.Failf("pass a duration like 7d, or a date like 2026-09-01",
					"could not read --since: %v", err)
			}
			if sinceTS >= untilTS {
				return out.Failf("pass a --since earlier than --until",
					"the range starts at or after it ends")
			}

			var res *pilots.UsageResponse
			if byMachine {
				res, err = client.Usage.ByMachine(c.Context(), sinceTS, untilTS)
			} else {
				res, err = client.Usage.Get(c.Context(), sinceTS, untilTS)
			}
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(res)
			}
			return renderUsage(env, res, byMachine)
		},
	}
	c.Flags().StringVar(&since, "since", "", "start of the range: a duration like 7d, or a date")
	c.Flags().StringVar(&until, "until", "", "end of the range; defaults to now")
	c.Flags().BoolVar(&byMachine, "by-machine", false, "break each org down by machine")

	Describe(c, Doc{
		When: "To see what has accrued, and with --by-machine, which machine\n" +
			"accrued it. Answers from ONE host: every host meters its own\n" +
			"machines and there is no aggregator, so a fleet's total is the sum\n" +
			"across hosts, which the dashboard does for you.",
		How: "Compute accrues while a machine runs. Storage -- the machine's\n" +
			"wall time, its volume and its checkpoints -- accrues in every\n" +
			"state, which is why a suspended machine is cheap rather than free.",
		Examples: []string{
			"pilot usage",
			"pilot usage --since 7d",
			"pilot usage --since 7d --by-machine",
			"pilot usage --json",
		},
	})
	return c
}

// renderUsage prints one row per org, then optionally one per machine.
func renderUsage(env *Env, res *pilots.UsageResponse, byMachine bool) error {
	orgs := make([]string, 0, len(res.Orgs))
	for org := range res.Orgs {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)

	rows := make([][]string, 0, len(orgs))
	for _, org := range orgs {
		rows = append(rows, usageRow(org, res.Orgs[org]))
	}
	if len(rows) == 0 {
		env.W.Linef("nothing metered in this range")
		return nil
	}
	if err := env.W.Table(usageHeaders("ORG"), rows); err != nil {
		return err
	}
	if !byMachine {
		return nil
	}

	for _, org := range orgs {
		machines := res.Machines[org]
		if len(machines) == 0 {
			continue
		}
		ids := make([]string, 0, len(machines))
		for id := range machines {
			ids = append(ids, id)
		}
		// By cost, not by name: the question is which machine is expensive.
		sort.Slice(ids, func(i, j int) bool {
			a, b := machines[ids[i]], machines[ids[j]]
			if a.MachineSeconds != b.MachineSeconds {
				return a.MachineSeconds > b.MachineSeconds
			}
			return ids[i] < ids[j]
		})
		machineRows := make([][]string, 0, len(ids))
		for _, id := range ids {
			machineRows = append(machineRows, usageRow(id, machines[id]))
		}
		env.W.Linef("")
		env.W.Linef("%s", org)
		if err := env.W.Table(usageHeaders("MACHINE"), machineRows); err != nil {
			return err
		}
	}
	return nil
}

func usageHeaders(first string) []string {
	return []string{first, "MACHINE-HOURS", "VCPU-HOURS", "GIB-HOURS", "VOLUME GIB-HOURS", "SNAPSHOT GIB-HOURS"}
}

// usageRow renders one accrual in hours.
//
// Hours rather than the seconds the wire carries: a month of one machine is
// 2.6 million seconds, and nobody reads that. The wire keeps seconds because
// that is what a billing system integrates over.
func usageRow(name string, t pilots.UsageTotals) []string {
	return []string{
		name,
		hours(t.MachineSeconds),
		hours(t.VCPUSeconds),
		hours(t.MiBSeconds / 1024),
		hours(t.VolumeGiBSeconds),
		hours(t.SnapshotGiBSeconds),
	}
}

func hours(seconds int64) string {
	if seconds == 0 {
		return "-"
	}
	h := float64(seconds) / 3600
	if h < 10 {
		return strconv.FormatFloat(h, 'f', 2, 64)
	}
	return strconv.FormatFloat(h, 'f', 1, 64)
}

// parseWhen reads a duration before now ("7d", "24h") or a date
// ("2026-09-01"), and falls back to def when empty.
//
// Both spellings because both are natural: a duration is what somebody typing
// asks for, a date is what somebody reconciling an invoice has.
func parseWhen(raw string, def time.Time) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def.Unix(), nil
	}
	// Days are not a time.Duration unit, and "7d" is the commonest thing
	// anybody types here.
	if rest, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.Atoi(rest); err == nil {
			return time.Now().AddDate(0, 0, -n).Unix(), nil
		}
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return time.Now().Add(-d).Unix(), nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.Unix(), nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		// A unix timestamp, which is what a script passes back from --json.
		return n, nil
	}
	return 0, out.Failf("pass a duration like 7d, a date like 2026-09-01, or a unix time",
		"%q is not a time", raw)
}
