package cli

import (
	"fmt"
	"os"
	"sync"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"
)

func newLogsCmd(env *Env) *cobra.Command {
	var (
		follow bool
		tail   int
	)
	c := &cobra.Command{
		Use:   "logs <service>",
		Short: "every replica's logs, each line prefixed with the replica name",
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
			machines, err := client.Machines.List(c.Context())
			if err != nil {
				return err
			}
			var replicas []pilots.Machine
			for _, m := range machines {
				if m.ServiceID == s.ID {
					replicas = append(replicas, m)
				}
			}
			if len(replicas) == 0 {
				env.W.Notef("%s has no replicas; `pilot deploy` or `pilot services set --replicas` gives it some", s.Name)
				return nil
			}
			width := 0
			for _, r := range replicas {
				if len(r.Name) > width {
					width = len(r.Name)
				}
			}

			// Every replica is read at once and lines are interleaved as they
			// arrive, with one writer so two replicas never tear a line.
			var mu sync.Mutex
			emit := func(name, line string) {
				mu.Lock()
				defer mu.Unlock()
				fmt.Fprintf(os.Stdout, "%-*s  %s\n", width, name, line)
			}

			if !follow {
				for _, r := range replicas {
					text, err := client.Machines.LogsTail(c.Context(), r.ID, tail)
					if err != nil {
						return err
					}
					for _, line := range splitLines(text) {
						emit(r.Name, line)
					}
				}
				return nil
			}

			errs := make(chan error, len(replicas))
			var wg sync.WaitGroup
			for _, r := range replicas {
				wg.Add(1)
				go func(r pilots.Machine) {
					defer wg.Done()
					lines, err := client.Machines.FollowLogs(c.Context(), r.ID)
					if err != nil {
						errs <- err
						return
					}
					for line, err := range lines {
						if err != nil {
							errs <- err
							return
						}
						emit(r.Name, line)
					}
				}(r)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil && c.Context().Err() == nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until interrupted")
	// The last N lines. What somebody wants nine times out of ten on a machine
	// that has been up for a week: the end, not the boot.
	c.Flags().IntVarP(&tail, "tail", "n", 0, "only the last N lines")
	Describe(c, Doc{
		When: "When a deploy's replica failed its health gate, or a service is\n" +
			"answering 5xx: the replicas' consoles say why. For one machine on its\n" +
			"own, `pilot machines logs`.",
		Examples: []string{
			"pilot logs web",
			"pilot logs web -f",
			"pilot logs web -f | grep -i error",
		},
	})
	return c
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}
