package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/coder/websocket"
)

// wsConn is the subset of the websocket connection the frame writer needs,
// kept as an interface so the framing logic is testable without a socket.
type wsConn interface {
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
}

const msgBinary = websocket.MessageBinary

// handleExecStream runs a command and streams its output frame by frame.
//
// This is the path a long-running agent command takes: output can be megabytes
// and the run can last minutes, so a buffered exec is unusable for it.
//
// Query parameters: repeated `cmd` (argv), `dir`, repeated `env` as K=V,
// `user`, and `stdin=true` to opt into forwarding client messages to the
// process. stdin is off by default -- most callers never write, and a process
// holding an open stdin it never reads can hang.
//
// Frames server to client are 1 stdout, 2 stderr, then the verdict: a text
// {"type":"exit","exit_code":n} and, after it, 3 exit. Client to server they
// are 0 stdin and 4 stdin EOF, and they are read only when the stream opted
// into stdin: with stdin=false nothing is read from the socket at all, so a 0
// frame sent anyway is ignored rather than an error.
//
// tty=true runs the command on a pseudo-terminal instead of three pipes, with
// `rows` and `cols` giving the initial window (24 by 80 by default, each
// 1..65535). It changes four things and nothing else:
//
//   - a PTY merges the two output streams, so everything the command writes
//     arrives as frame 1 and frame 2 is NEVER sent;
//   - a tty implies stdin, so client frames are read whether or not stdin=true
//     was asked for (hostd refuses the contradictory stdin=false up front);
//   - frame 4 writes EOT (0x04) to the terminal rather than closing anything,
//     because a terminal has no separate stdin to close, and the session stays
//     open: what EOT means is the shell's decision, not this agent's;
//   - a TEXT message {"type":"resize","cols":N,"rows":N} resizes the window.
//
// The exit verdict is unchanged, so a client that only reads frames needs no
// tty-specific code to learn how the command ended.
func handleExecStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	argv := q["cmd"]
	if len(argv) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cmd is required"})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The connection is authenticated by token before it gets here, and
		// the client is an SDK rather than a browser, so Origin checking adds
		// nothing.
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("guest-agent: ws accept: %v", err)
		return
	}
	defer conn.CloseNow()

	// After the upgrade, so a bad window size is reported as a close code the
	// client is already listening for rather than an HTTP status it can no
	// longer see.
	tty := q.Get("tty") == "true"
	rows, rowsOK := winDim(q.Get("rows"), 24)
	cols, colsOK := winDim(q.Get("cols"), 80)
	if !rowsOK || !colsOK {
		_ = conn.Close(websocket.StatusPolicyViolation, "rows and cols must be 1..65535")
		return
	}

	// Detached from the HTTP request context: the command should outlive the
	// handler's own lifetime bookkeeping and end only when it exits or the
	// socket closes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := prepareCommand(cmd, q.Get("user"), q.Get("dir"), nil); err != nil {
		_ = conn.Close(websocket.StatusInternalError, err.Error())
		return
	}
	// After prepareCommand, so the caller's env overrides the account's
	// defaults -- for duplicate keys the last entry wins.
	cmd.Env = append(cmd.Env, q["env"]...)

	fw := &frameWriter{conn: conn, ctx: ctx}

	var (
		stdin io.WriteCloser
		ptmx  *os.File
		pumps sync.WaitGroup
	)

	if tty {
		// One pump, not two: the PTY master IS both output streams, so a
		// second pump would have nothing to read and frame 2 would never be
		// sent anyway. It is stdin as well -- a terminal has one device.
		if ptmx, err = startPTY(cmd, rows, cols); err != nil {
			// 127 is the shell's convention for "could not run it", and is
			// distinguishable from any exit code the command itself could
			// return.
			writeExit(fw, 127)
			_ = conn.Close(websocket.StatusNormalClosure, "start failed")
			return
		}
		defer ptmx.Close()
		pumps.Add(1)
		go pump(fw, frameStdout, ptmx, pumps.Done)
	} else {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "stdout pipe")
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "stderr pipe")
			return
		}

		if q.Get("stdin") == "true" {
			if stdin, err = cmd.StdinPipe(); err != nil {
				_ = conn.Close(websocket.StatusInternalError, "stdin pipe")
				return
			}
		}

		if err := cmd.Start(); err != nil {
			writeExit(fw, 127)
			_ = conn.Close(websocket.StatusNormalClosure, "start failed")
			return
		}

		pumps.Add(2)
		go pump(fw, frameStdout, stdout, pumps.Done)
		go pump(fw, frameStderr, stderr, pumps.Done)
	}
	defer trackPID(cmd.Process)()

	// A tty implies stdin: there is no way to type into a terminal otherwise,
	// and hostd refuses tty=true with stdin=false before the machine is woken.
	if stdin != nil || ptmx != nil {
		go func() {
			if stdin != nil {
				defer stdin.Close()
			}
			for {
				typ, data, err := conn.Read(ctx)
				if err != nil {
					// A pipe stream ends itself: the deferred stdin.Close
					// delivers EOF to the process. A terminal has no such end,
					// and an interactive shell writes nothing while it waits
					// for input, so the output pump would park on the master
					// forever and the handler with it -- one orphaned shell,
					// two goroutines and a PTY per closed browser tab. The
					// cancel is what the /terminal handler has always done.
					if ptmx != nil {
						cancel()
					}
					return
				}
				// Text frames carry control messages. Only resize is
				// implemented, and only a tty has a window to resize, so
				// without one this is still the drop it always was.
				if typ == websocket.MessageText {
					var ctl struct {
						Type string `json:"type"`
						Cols uint16 `json:"cols"`
						Rows uint16 `json:"rows"`
					}
					if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" {
						resizePTY(ptmx, ctl.Cols, ctl.Rows)
					}
					continue
				}
				// The sprites frame protocol, client to server: byte 0 is the
				// stream id. An empty frame and any id this agent does not know
				// are ignored, which means a legacy raw-stdin client is refused
				// rather than guessed at.
				if typ != websocket.MessageBinary || len(data) == 0 {
					continue
				}
				switch data[0] {
				case frameStdin:
					if _, err := writeStdin(stdin, ptmx, data[1:]); err != nil {
						return
					}
				case frameStdinEOF:
					if ptmx != nil {
						// A terminal has no write end to close, so end-of-file
						// is the EOT character the line discipline turns into
						// one for whatever is reading. The session stays open:
						// what EOT means there is the shell's decision.
						_, _ = ptmx.Write([]byte{4})
						continue
					}
					return // the deferred Close delivers EOF to the process
				}
			}
		}()
	}

	// Both pumps must drain before the exit frame goes out, or a client can
	// see the exit code before the output that preceded it.
	pumps.Wait()
	writeExit(fw, exitCodeOf(cmd.Wait()))
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// writeExit sends the verdict twice: as text, and then as the binary frame.
//
// The order is the whole point. Websocket frames are ordered, and both SDKs
// act on whichever verdict reaches them first -- they close the socket and
// return -- so a text frame written AFTER the binary one is a frame nothing
// can ever receive. It went out second for a while, which made its documented
// purpose unachievable.
//
// That purpose is real: the binary frame carries the code in one byte, and a
// command killed by a signal has an ExitCode of -1, which byte() reports as
// 255 -- indistinguishable from a command that genuinely exited 255. The text
// form carries the code as an integer, so it has to be the one that lands
// first.
//
// The binary frame still follows, because it is the sprites byte protocol and
// a client that reads only binary frames is the whole reason to keep it.
func writeExit(fw *frameWriter, code int) {
	_ = fw.writeText(fmt.Appendf(nil, `{"type":"exit","exit_code":%d}`, code))
	_ = fw.write(frameExit, []byte{byte(code)})
}

// winDim parses a rows or cols query value. An absent value takes the default;
// anything that is not 1..65535 is refused rather than clamped, because a
// client that computed a window size wrongly should learn that at the
// handshake and not from a terminal that silently drew itself 80 columns wide.
func winDim(raw string, def uint16) (uint16, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint16(n), true
}

// writeStdin sends one client chunk to whichever input the stream opened.
func writeStdin(stdin io.WriteCloser, ptmx *os.File, chunk []byte) (int, error) {
	if ptmx != nil {
		return ptmx.Write(chunk)
	}
	return stdin.Write(chunk)
}
