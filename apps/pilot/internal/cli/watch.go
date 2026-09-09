package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// first is the optional single argument of a command that also accepts the
// .pilot context or --select in its place.
func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// watch re-renders every rate seconds until interrupted, clearing the screen
// between renders so the table stays in place rather than scrolling. Only on
// a terminal: piped output gets one render, because a pipeline that wanted
// live updates would have asked for --json and polled.
func watch(c *cobra.Command, env *Env, rate int, render func() error) error {
	if env.W.JSON {
		return out.Failf("poll `--json` from a script instead", "--watch and --json do not combine")
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return render()
	}
	if rate < 1 {
		rate = 1
	}
	for {
		fmt.Fprint(os.Stdout, "\033[H\033[2J")
		if err := render(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "\nevery %ds · %s · ctrl-c to stop\n", rate, time.Now().Format("15:04:05"))
		select {
		case <-c.Context().Done():
			return nil
		case <-time.After(time.Duration(rate) * time.Second):
		}
	}
}
