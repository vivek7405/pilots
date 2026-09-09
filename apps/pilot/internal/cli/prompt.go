package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// confirm asks before something irreversible. -y answers for a script or an
// agent; otherwise the question goes to stderr and the answer is read from
// the terminal. Not a terminal and no -y is a refusal that says so, rather
// than a hang waiting on stdin that will never speak: an agent that forgot
// -y gets told what to pass, and a pipeline never blocks.
func confirm(env *Env, question string) error {
	if env.Yes {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return out.Failf("pass -y to confirm without a terminal", "stdin is not a terminal, so there is nothing to ask on")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr)
		return out.Failf("answer y, or pass -y", "no answer")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return out.Failf("nothing was changed", "cancelled")
}
