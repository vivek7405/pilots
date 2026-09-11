package cli

import (
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// detachKey is ctrl-\ (0x1c): the one byte a console intercepts. Chosen
// over ctrl-d because ctrl-d is what a shell treats as EOF, and over
// tmux's prefix because a console should not eat a whole key family.
const detachKey = 0x1c

func newAttachCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "attach [machine] [session]",
		Short: "reconnect to a console session that is still running",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := machineArg(c, env, client, first(args))
			if err != nil {
				return err
			}
			sessionID := ""
			if len(args) == 2 {
				sessionID = args[1]
			} else {
				sessions, err := client.Machines.Sessions(c.Context(), m.ID)
				if err != nil {
					return err
				}
				for i := len(sessions) - 1; i >= 0; i-- {
					if !sessions[i].Ended {
						sessionID = sessions[i].ID
						break
					}
				}
				if sessionID == "" {
					return out.Failf("pilot console opens a new one; pilot sessions ls shows what there is", "no live session on %s", m.Name)
				}
			}
			return runAttach(c, env, client, m.ID, sessionID)
		},
	}
	Describe(c, Doc{
		What: "A console is a session that keeps running when you disconnect. Close\n" +
			"the laptop, lose the link, press ctrl-\\ to detach on purpose: the\n" +
			"shell and whatever it was running carry on. attach puts you back in\n" +
			"it, replaying what it printed while nobody was watching.",
		When: "- `pilot console` -- start a new session\n" +
			"- `pilot attach`  -- return to the most recent live one, or a named one\n" +
			"- `pilot exec`    -- one command, no session",
		Examples: []string{
			"pilot attach",
			"pilot attach scratch",
			"pilot attach scratch s-1a2b3c4d",
		},
		Related: []string{
			"pilot sessions ls     every session on a machine",
			"pilot sessions kill   end one that should stop",
		},
	})
	return c
}

func newSessionsCmd(env *Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "sessions",
		Short: "the console sessions a machine has open",
	}
	ls := &cobra.Command{
		Use:     "list [machine]",
		Aliases: []string{"ls"},
		Short:   "list sessions",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := machineArg(c, env, client, first(args))
			if err != nil {
				return err
			}
			sessions, err := client.Machines.Sessions(c.Context(), m.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(sessions)
			}
			if len(sessions) == 0 {
				env.W.Notef("no sessions on %s; `pilot console` starts one", m.Name)
				return nil
			}
			rows := make([][]string, 0, len(sessions))
			for _, s := range sessions {
				state := "live"
				if s.Ended {
					state = "ended " + strconv.Itoa(s.ExitCode)
				} else if s.Attached {
					state = "attached"
				}
				// A session running a command keeps the machine awake; say
				// so where the operator is looking for why it has not slept.
				if !s.Ended && s.Busy {
					state += ", busy"
				}
				rows = append(rows, []string{s.ID, state, unixTime(s.CreatedAt), strings.Join(s.Argv, " ")})
			}
			return env.W.Table([]string{"SESSION", "STATE", "STARTED", "COMMAND"}, rows)
		},
	}
	Describe(ls, Doc{Examples: []string{"pilot sessions ls", "pilot sessions ls scratch --json"}})
	kill := &cobra.Command{
		Use:   "kill <session> [machine]",
		Short: "end a session's process",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := machineArg(c, env, client, first(args[1:]))
			if err != nil {
				return err
			}
			if err := client.Machines.KillSession(c.Context(), m.ID, args[0]); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"killed": args[0]})
			}
			env.W.Linef("killed %s", args[0])
			return nil
		},
	}
	Describe(kill, Doc{
		Warning:  "Whatever the session was running is killed with it.",
		Examples: []string{"pilot sessions kill s-1a2b3c4d"},
	})
	root.AddCommand(ls, kill)
	return root
}

// runAttach is runConsole for an existing session.
func runAttach(c *cobra.Command, env *Env, client *pilots.Client, id, session string) error {
	stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(stdinFd) || !term.IsTerminal(stdoutFd) {
		return out.Failf("run it from a terminal", "attach needs stdin and stdout to be a terminal")
	}
	cols, rows, err := term.GetSize(stdoutFd)
	if err != nil {
		cols, rows = 80, 24
	}
	stream, err := client.Machines.Attach(c.Context(), id, session, uint16(rows), uint16(cols))
	if err != nil {
		return err
	}
	return driveTerminal(env, stream, stdinFd, stdoutFd, session)
}

// driveTerminal is the raw-mode loop shared by console and attach: keys go
// to the session, output comes back, the window size follows, and ctrl-\
// detaches. It restores the terminal on every exit path.
func driveTerminal(env *Env, stream *pilots.ExecStream, stdinFd, stdoutFd int, sessionID string) error {
	defer stream.Close()
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return err
	}
	defer term.Restore(stdinFd, oldState)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if c, r, err := term.GetSize(stdoutFd); err == nil {
				_ = stream.Resize(uint16(c), uint16(r))
			}
		}
	}()

	// The session id arrives in the first frames; say it once, on stderr,
	// so the person knows what `pilot attach` would take.
	go func() {
		for i := 0; i < 50; i++ {
			if id := stream.SessionID(); id != "" {
				if sessionID == "" {
					env.W.Notef("\r\nsession %s · ctrl-\\ detaches, `pilot attach` returns\r", id)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	detached := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if i := indexByte(chunk, detachKey); i >= 0 {
					if i > 0 {
						_, _ = stream.Stdin.Write(chunk[:i])
					}
					// Say "detaching" BEFORE closing the stream: closing ends
					// the output copy too, and the main loop must read that
					// as a detach rather than as a stream that died.
					close(detached)
					_ = stream.Detach()
					return
				}
				if _, werr := stream.Stdin.Write(chunk); werr != nil {
					return
				}
			}
			if err != nil {
				_ = stream.Stdin.Close()
				return
			}
		}
	}()
	outDone := make(chan struct{})
	go func() { _, _ = io.Copy(os.Stdout, stream.Stdout); close(outDone) }()

	<-outDone
	// The output copy ends for one of two reasons: the session ended, or we
	// detached and closed the stream ourselves. The detach signal is raised
	// before the close, so it is readable here without a race.
	select {
	case <-detached:
		term.Restore(stdinFd, oldState)
		env.W.Notef("\r\ndetached; the session keeps running. `pilot attach` returns to it")
		return nil
	default:
	}
	code, err := stream.Wait()
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
