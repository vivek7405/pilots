// Command pilot is the pilots CLI.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/vivek7405/pilots/cli/internal/cli"
	"github.com/vivek7405/pilots/cli/internal/config"
)

func main() {
	// Ctrl-C ends the command rather than being swallowed by a stream: a
	// `pilot logs --follow` that ignores an interrupt is a terminal an
	// operator has to close.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := cli.Execute(ctx, config.OSEnv, os.Args[1:])

	// The reader of stdout can go away first: `pilot machines ls | head -3`,
	// a `grep -q` that has seen enough, a `less` the operator quit. Go leaves
	// SIGPIPE fatal for stdout and stderr, so that case usually ends the
	// process with the right signal on its own. What reaches here is the
	// error surfacing through a write we handled: it exits 141, which is
	// 128 + SIGPIPE and the code a shell already reports for the left side of
	// `seq 1 100000 | head -1`. Exiting quietly matters because stderr is
	// supposed to carry the server's refusal and nothing else -- a stack
	// trace there would make "nobody was listening" look like "the fleet said
	// no". This is the same bug packages/cli carried until #96.
	if code != 0 {
		if err := ctx.Err(); errors.Is(err, context.Canceled) {
			os.Exit(130) // 128 + SIGINT
		}
	}
	os.Exit(code)
}
