package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
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

	// 0, 0 keeps the kernel's default window until the client sends its first
	// resize, which is what this handler has always done.
	ptmx, err := startPTY(cmd, 0, 0)
	if err != nil {
		tw.send(terminalFrame{Type: "error", Data: err.Error()})
		_ = conn.Close(websocket.StatusInternalError, "pty start failed")
		return
	}
	defer ptmx.Close()
	defer trackPID(cmd.Process)()

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
			resizePTY(ptmx, frame.Cols, frame.Rows)
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

// startPTY runs cmd on a new pseudo-terminal and returns the master side.
//
// Shared with the exec stream's tty mode so one place knows how a PTY is
// opened: the size is applied BEFORE the command starts, because a shell reads
// its window size at startup and a resize that lands after it has already
// drawn a prompt is a redraw the caller can see. rows or cols of 0 leaves the
// kernel default in place.
func startPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	if rows == 0 || cols == 0 {
		return pty.Start(cmd)
	}
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
}

// resizePTY applies a window size to an open PTY.
//
// A failure is deliberately not fatal and not reported: the only ways this
// fails are a PTY the process already closed and a size the kernel refuses,
// and neither is worth ending a live session over. A nil master (the exec
// stream without tty) is a no-op, which is what lets the read loop forward a
// resize message unconditionally.
func resizePTY(ptmx *os.File, cols, rows uint16) {
	if ptmx == nil || cols == 0 || rows == 0 {
		return
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}
