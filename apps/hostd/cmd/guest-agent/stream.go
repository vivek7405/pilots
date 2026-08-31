package main

import (
	"context"
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
		_ = fw.write(frameExit, []byte{127})
		_ = conn.Close(websocket.StatusNormalClosure, "start failed")
		return
	}

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
				if typ == websocket.MessageBinary || typ == websocket.MessageText {
					if _, err := stdin.Write(data); err != nil {
						return
					}
				}
			}
		}()
	}

	// Both pumps must drain before the exit frame goes out, or a client can
	// see the exit code before the output that preceded it.
	pumps.Wait()
	code := exitCodeOf(cmd.Wait())

	_ = fw.write(frameExit, []byte{byte(code)})
	_ = conn.Close(websocket.StatusNormalClosure, "")
}
