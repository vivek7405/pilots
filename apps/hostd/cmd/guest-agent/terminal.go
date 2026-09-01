package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
	"sync"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// terminalFrame is the JSON envelope for an interactive session. Unlike exec
// streaming, a terminal is inherently text and bidirectional, so a structured
// frame is easier to work with than a byte protocol.
type terminalFrame struct {
	Type string `json:"type"`           // session|data|resize|exit|error
	Data string `json:"data,omitempty"` // base64 for data frames
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Code int    `json:"code,omitempty"`
}

// handleTerminal gives an interactive shell over a websocket.
func handleTerminal(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("guest-agent: ws accept: %v", err)
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, guestShell(), "-l")
	tw := &termWriter{conn: conn, ctx: ctx}

	if err := prepareCommand(cmd, r.URL.Query().Get("user"), r.URL.Query().Get("dir"), nil); err != nil {
		tw.send(terminalFrame{Type: "error", Data: err.Error()})
		_ = conn.Close(websocket.StatusInternalError, "prepare failed")
		return
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		tw.send(terminalFrame{Type: "error", Data: err.Error()})
		_ = conn.Close(websocket.StatusInternalError, "pty start failed")
		return
	}
	defer ptmx.Close()

	tw.send(terminalFrame{Type: "session"})

	// PTY -> client. On EOF this MUST close the connection: without it the
	// read loop below blocks forever and `exit` inside the guest leaves the
	// caller hanging with no indication the shell is gone.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				tw.send(terminalFrame{
					Type: "data",
					Data: base64.StdEncoding.EncodeToString(buf[:n]),
				})
			}
			if err != nil {
				code := exitCodeOf(cmd.Wait())
				tw.send(terminalFrame{Type: "exit", Code: code})
				_ = conn.Close(websocket.StatusNormalClosure, "")
				cancel()
				return
			}
		}
	}()

	// Client -> PTY.
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var frame terminalFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		switch frame.Type {
		case "data":
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				continue
			}
			if _, err := ptmx.Write(data); err != nil {
				return
			}
		case "resize":
			_ = pty.Setsize(ptmx, &pty.Winsize{Cols: frame.Cols, Rows: frame.Rows})
		}
	}
}

// termWriter serialises writes for ONE session. The PTY reader goroutine and
// the main read loop both emit frames, and concurrent websocket writes are
// forbidden -- but the lock must be per-connection, or every terminal session
// in the machine would block on the same mutex.
type termWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
	ctx  context.Context
}

func (tw *termWriter) send(f terminalFrame) {
	payload, err := json.Marshal(f)
	if err != nil {
		return
	}
	tw.mu.Lock()
	defer tw.mu.Unlock()
	_ = tw.conn.Write(tw.ctx, websocket.MessageText, payload)
}
