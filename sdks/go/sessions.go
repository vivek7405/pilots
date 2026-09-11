package pilots

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"
)

// Session is a terminal session inside a machine: a shell (or any command
// run with a TTY) that outlives the connection that opened it. Attach to it
// by id to see what it printed while nobody was watching and carry on.
type Session struct {
	ID        string   `json:"id"`
	Argv      []string `json:"argv"`
	CreatedAt int64    `json:"created_at"`
	Attached  bool     `json:"attached"`
	// Busy is whether a command is running in the session right now, read
	// from the guest's process tree: a shell at its prompt is not busy, a
	// build it started is, attached or not. A busy session keeps the
	// machine awake.
	Busy     bool `json:"busy"`
	Ended    bool `json:"ended"`
	ExitCode int  `json:"exit_code"`
}

// Sessions lists a machine's terminal sessions, oldest first, including
// ones that ended in the last few minutes.
func (m *Machines) Sessions(ctx context.Context, id string) ([]Session, error) {
	var out []Session
	if err := m.c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/sessions", nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Session{}
	}
	return out, nil
}

// KillSession ends a session's process.
func (m *Machines) KillSession(ctx context.Context, id, session string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/sessions/"+url.PathEscape(session)+"/kill", nil, nil)
}

// Attach reconnects to a session. The stream carries the same frames an
// exec stream with a TTY does: scrollback first, then live output, and the
// exit frame when the process ends. Stdin, Resize and Detach work as on a
// fresh console.
func (m *Machines) Attach(ctx context.Context, id, session string, rows, cols uint16) (*ExecStream, error) {
	target := replaceScheme(m.c.baseURL) + "/v1/machines/" + url.PathEscape(id) + "/attach/" + url.PathEscape(session)
	q := url.Values{}
	if m.c.org != "" {
		q.Set("org", m.c.org)
	}
	q.Set("tty", "true")
	if rows > 0 && cols > 0 {
		q.Set("rows", strconv.Itoa(int(rows)))
		q.Set("cols", strconv.Itoa(int(cols)))
	}
	target += "?" + q.Encode()
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	conn, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + m.c.apiKey}},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	s := &ExecStream{
		Stdout: stdoutR, Stderr: stderrR,
		stdoutR: stdoutR, stderrR: stderrR,
		conn: conn, cancel: cancel, done: make(chan struct{}), code: -1,
		ctx: streamCtx, tty: true, sessionID: session,
	}
	s.Stdin = &stdinWriter{conn: conn, ctx: streamCtx}
	go s.read(streamCtx, stdoutW, stderrW)
	return s, nil
}

// SessionID is the id of the terminal session behind a TTY stream, once the
// agent has announced it (it arrives in the first frames); empty for a
// non-TTY stream.
func (s *ExecStream) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Detach leaves the session running and ends this stream. It is what a
// console does on ctrl-\: the shell keeps going, `pilot attach` returns to
// it. On a non-TTY stream it is the same as Close.
func (s *ExecStream) Detach() error {
	if !s.tty {
		return s.Close()
	}
	msg, _ := json.Marshal(map[string]string{"type": "detach"})
	_ = s.conn.Write(s.ctx, websocket.MessageText, msg)
	return s.Close()
}

func replaceScheme(baseURL string) string {
	if len(baseURL) >= 5 && baseURL[:5] == "https" {
		return "wss" + baseURL[5:]
	}
	if len(baseURL) >= 4 && baseURL[:4] == "http" {
		return "ws" + baseURL[4:]
	}
	return baseURL
}
