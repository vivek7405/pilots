package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
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

	var stdin io.WriteCloser
	if q.Get("stdin") == "true" {
		if stdin, err = cmd.StdinPipe(); err != nil {
			_ = conn.Close(websocket.StatusInternalError, "stdin pipe")
			return
		}
	}

	if err := cmd.Start(); err != nil {
		// 127 is the shell's convention for "could not run it", and is
		// distinguishable from any exit code the command itself could return.
		writeExit(fw, 127)
		_ = conn.Close(websocket.StatusNormalClosure, "start failed")
		return
	}
	defer trackPID(cmd.Process)()

	var pumps sync.WaitGroup
	pumps.Add(2)
	go pump(fw, frameStdout, stdout, pumps.Done)
	go pump(fw, frameStderr, stderr, pumps.Done)

	if stdin != nil {
		go func() {
			defer stdin.Close()
			for {
				typ, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				// The sprites frame protocol, client to server: byte 0 is the
				// stream id. Text frames carry control messages this agent does
				// not implement (resize, port events) and are ignored; so is an
				// empty frame and any id this agent does not know, which means a
				// legacy raw-stdin client is refused rather than guessed at.
				if typ != websocket.MessageBinary || len(data) == 0 {
					continue
				}
				switch data[0] {
				case frameStdin:
					if _, err := stdin.Write(data[1:]); err != nil {
						return
					}
				case frameStdinEOF:
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
