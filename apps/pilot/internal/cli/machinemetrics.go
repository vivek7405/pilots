package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"
)

// What a machine is using, and what a service's instances are using together.
//
// # Why this is not `pilot metrics`
//
// `pilot metrics <service>` already exists and answers a different question:
// what the DATABASE ENGINE inside a service says about itself. These are the
// platform's own numbers about the machine. Two different questions under one
// name would be worse than a longer name, so these live under the object they
// describe.

func newMachineMetricsCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "metrics <machine>",
		Short: "what this machine is using",
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
			got, err := client.Machines.Metrics(c.Context(), m.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(got)
			}
			return env.W.Table([]string{"", ""}, metricRows(got))
		},
	}
	Describe(c, Doc{
		What: "CPU seconds used, memory held now, and the ceilings for both.",
		How: "Read from the machine's cgroup on the host that owns it, so these are\n" +
			"the kernel's numbers rather than an estimate.\n\n" +
			"CPU is a total rather than a rate, and it does not go backwards across\n" +
			"a suspend. Two readings a minute apart give you the rate.\n\n" +
			"Memory is zero while a machine is suspended. That is the truth: a\n" +
			"suspended machine holds no memory, which is the whole point of\n" +
			"suspending it.",
		Related: []string{
			"pilot metrics    what a DATABASE engine says about itself",
			"pilot machines logs   what it printed",
		},
		Examples: []string{
			"pilot machines metrics m-123",
			"pilot machines metrics m-123 --json",
		},
	})
	return c
}

func newServiceMetricsCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "metrics <service>",
		Short: "what each instance of this service is using",
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
			machines, err := client.Machines.List(c.Context())
			if err != nil {
				return err
			}

			var got []*pilots.MachineMetrics
			for i := range machines {
				if machines[i].ServiceID != svc.ID {
					continue
				}
				// One call per instance rather than a scrape, because a scrape
				// would be the whole org and this was asked about one service.
				// Each lands on that instance's own host.
				one, err := client.Machines.Metrics(c.Context(), machines[i].ID)
				if err != nil {
					// One instance that cannot be read must not hide the rest:
					// a partial answer is what somebody looking at a struggling
					// service actually needs.
					env.W.Notef("could not read %s: %v", machines[i].Name, err)
					continue
				}
				got = append(got, one)
			}
			if len(got) == 0 {
				return env.W.Table([]string{"INSTANCE", "STATE", "CPU", "MEMORY"}, nil)
			}
			if env.W.JSON {
				return env.W.JSONValue(got)
			}

			rows := make([][]string, 0, len(got))
			for _, one := range got {
				rows = append(rows, []string{
					orDash(one.Name), one.State,
					fmt.Sprintf("%.1fs", one.CPUSeconds),
					memoryLine(one),
				})
			}
			return env.W.Table([]string{"INSTANCE", "STATE", "CPU", "MEMORY"}, rows)
		},
	}
	Describe(c, Doc{
		What: "One row per instance: what each is using and what it is held to.",
		How: "An instance that cannot be read is reported and skipped rather than\n" +
			"failing the whole table. A partial answer is what somebody looking at\n" +
			"a struggling service actually needs.",
		Related: []string{
			"pilot metrics    what a DATABASE engine says about itself",
			"pilot services info   what the service is",
		},
		Examples: []string{"pilot services metrics web"},
	})
	return c
}

func metricRows(m *pilots.MachineMetrics) [][]string {
	rows := [][]string{
		{"MACHINE", m.MachineID},
		{"STATE", m.State},
		{"VCPUS", strconv.Itoa(m.VCPUs)},
		{"CPU", fmt.Sprintf("%.1fs total", m.CPUSeconds)},
		{"MEMORY", memoryLine(m)},
	}
	if m.ServiceID != "" {
		rows = append(rows, []string{"SERVICE", m.ServiceID})
	}
	return rows
}

// memoryLine puts the number next to its ceiling, and says when it is close.
//
// "412 MiB" means nothing on its own. "412 MiB / 512 MiB" means something, and
// at 90 percent the sentence is what tells somebody the next allocation is the
// one that gets the process killed.
func memoryLine(m *pilots.MachineMetrics) string {
	if m.MemoryLimitBytes <= 0 {
		return humanBytes(m.MemoryBytes)
	}
	line := humanBytes(m.MemoryBytes) + " / " + humanBytes(m.MemoryLimitBytes)
	if m.State == "suspended" {
		return line + "  (suspended, so it holds none)"
	}
	if float64(m.MemoryBytes) >= 0.9*float64(m.MemoryLimitBytes) {
		line += "  (near the ceiling; the next allocation may be the one that fails)"
	}
	return line
}
