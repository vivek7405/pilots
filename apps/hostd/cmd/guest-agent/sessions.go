package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// A terminal session outlives the client that opened it.
//
// Before this, a tty exec stream ran its shell under the connection's
// context: the websocket dropping -- a laptop lid, a flaky link, an agent
// that exited -- killed the shell and whatever it was running. Now a tty
// session runs under its own context, keeps the last scrollback in a ring
// buffer, and a client that comes back attaches by id, sees what it
// missed, and carries on. The process ends only when it exits or is
// killed; the machine being destroyed ends everything.
//
// Non-tty streams are unchanged: a piped command with no terminal has no
// session to come back to, and its exit status is the whole answer.

const (
	scrollbackBytes = 256 << 10
	endedSessionTTL = 10 * time.Minute
)

type session struct {
	ID        string    `json:"id"`
	Argv      []string  `json:"argv"`
	CreatedAt time.Time `json:"created_at"`
	Attached  bool      `json:"attached"`
	Ended     bool      `json:"ended"`
	ExitCode  int       `json:"exit_code"`
	EndedAt   time.Time `json:"ended_at,omitempty"`

	mu         sync.Mutex
	ptmx       *os.File
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	ring       []byte // last scrollbackBytes of output
	client     *frameWriter
	clientConn *websocket.Conn
	done       chan struct{}
}

var sessions = struct {
	mu sync.Mutex
	m  map[string]*session
}{m: map[string]*session{}}

func newSessionID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "s-" + hex.EncodeToString(b[:])
}

// startSession runs argv on a PTY under a context nobody's connection owns.
func startSession(argv []string, q url.Values, rows, cols uint16) (*session, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := prepareCommand(cmd, q.Get("user"), q.Get("dir"), nil); err != nil {
		cancel()
		return nil, err
	}
	cmd.Env = append(cmd.Env, q["env"]...)
	ptmx, err := startPTY(cmd, rows, cols)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &session{ID: newSessionID(), Argv: argv, CreatedAt: time.Now(), ptmx: ptmx, cmd: cmd, cancel: cancel, done: make(chan struct{})}
	untrack := trackPID(cmd.Process)
	sessions.mu.Lock()
	sessions.m[s.ID] = s
	sessions.mu.Unlock()

	// One reader for the PTY for the life of the process: into the ring,
	// and to whichever client is attached at the moment.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.ring = append(s.ring, buf[:n]...)
				if len(s.ring) > scrollbackBytes {
					s.ring = s.ring[len(s.ring)-scrollbackBytes:]
				}
				client := s.client
				s.mu.Unlock()
				if client != nil {
					if werr := client.write(frameStdout, buf[:n]); werr != nil {
						s.detach(client)
					}
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) {
					log.Printf("guest-agent: session %s read: %v", s.ID, err)
				}
				break
			}
		}
		code := exitCodeOf(cmd.Wait())
		untrack()
		ptmx.Close()
		s.mu.Lock()
		s.Ended, s.ExitCode, s.EndedAt = true, code, time.Now()
		client, clientConn := s.client, s.clientConn
		s.client, s.clientConn = nil, nil
		s.mu.Unlock()
		if client != nil {
			writeExit(client, code)
			_ = clientConn.Close(websocket.StatusNormalClosure, "")
		}
		close(s.done)
		// Keep the record briefly so a client that reconnects learns how it
		// ended, then forget it.
		time.AfterFunc(endedSessionTTL, func() {
			sessions.mu.Lock()
			delete(sessions.m, s.ID)
			sessions.mu.Unlock()
		})
	}()
	return s, nil
}

// attach binds a client, replacing any previous one (the newer terminal
// wins, the way tmux attach -d does), replays the scrollback, and pumps
// the client's frames into the PTY until the client goes away.
func (s *session) attach(conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fw := &frameWriter{conn: conn, ctx: ctx}

	// The scrollback is replayed UNDER the session lock, with the client
	// already installed. Replaying it after the unlock would race the PTY
	// reader, which takes the same lock to append and then writes to whatever
	// client it found: on a session that is still printing, the new client
	// sees a live chunk before the history that precedes it.
	s.mu.Lock()
	prevConn := s.clientConn
	s.client, s.clientConn = fw, conn
	s.Attached = true
	ended, code := s.Ended, s.ExitCode
	_ = fw.writeText(fmt.Appendf(nil, `{"type":"session","id":%q}`, s.ID))
	if len(s.ring) > 0 {
		_ = fw.write(frameStdout, s.ring)
	}
	s.mu.Unlock()
	if prevConn != nil {
		_ = prevConn.Close(websocket.StatusNormalClosure, "attached elsewhere")
	}

	if ended {
		writeExit(fw, code)
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return
	}

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			s.detach(fw) // the client left; the shell stays
			return
		}
		if typ == websocket.MessageText {
			var ctl struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &ctl) == nil {
				switch ctl.Type {
				case "resize":
					resizePTY(s.ptmx, ctl.Cols, ctl.Rows)
				case "detach":
					s.detach(fw)
					_ = conn.Close(websocket.StatusNormalClosure, "detached")
					return
				}
			}
			continue
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameStdin:
			if _, err := s.ptmx.Write(data[1:]); err != nil {
				s.detach(fw)
				return
			}
		case frameStdinEOF:
			_, _ = s.ptmx.Write([]byte{4})
		}
	}
}

func (s *session) detach(fw *frameWriter) {
	s.mu.Lock()
	if s.client == fw {
		s.client, s.clientConn = nil, nil
		s.Attached = false
	}
	s.mu.Unlock()
}

func (s *session) kill() {
	s.cancel()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func lookupSession(id string) (*session, bool) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	s, ok := sessions.m[id]
	return s, ok
}

// serveTTYSession is the tty half of GET /exec/stream: start a session and
// attach the opening client to it.
func serveTTYSession(conn *websocket.Conn, argv []string, q url.Values, rows, cols uint16) {
	s, err := startSession(argv, q, rows, cols)
	if err != nil {
		fw := &frameWriter{conn: conn, ctx: context.Background()}
		writeExit(fw, 127)
		_ = conn.Close(websocket.StatusNormalClosure, "start failed")
		return
	}
	s.attach(conn)
}

// handleSessions is GET /sessions: every session, live or recently ended.
func handleSessions(w http.ResponseWriter, _ *http.Request) {
	sessions.mu.Lock()
	list := make([]*session, 0, len(sessions.m))
	for _, s := range sessions.m {
		list = append(list, s)
	}
	sessions.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	// busy is read from the process tree (busy.go), once for every session
	// and outside the session locks: the scan walks /proc and must not hold
	// up the PTY reader.
	busy := busySessions(procRoot)
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		s.mu.Lock()
		ended, leader := s.Ended, 0
		if s.cmd != nil && s.cmd.Process != nil {
			leader = s.cmd.Process.Pid
		}
		entry := map[string]any{
			"id": s.ID, "argv": s.Argv, "created_at": s.CreatedAt.Unix(), "attached": s.Attached,
			"ended": ended, "exit_code": s.ExitCode,
		}
		s.mu.Unlock()
		entry["busy"] = !ended && leader != 0 && busy[leader]
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAttach is GET /attach?session=<id>&rows=&cols=.
//
// The window is resized to the ARRIVING terminal's, because a session is
// attached to from a different terminal than it was opened in as a matter of
// course -- another laptop, a wider window -- and a full-screen program drawn
// to the old size is unreadable until something happens to raise SIGWINCH.
func handleAttach(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := lookupSession(q.Get("session"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("guest-agent: attach: ws accept: %v", err)
		return
	}
	defer conn.CloseNow()
	rows, rowsOK := winDim(q.Get("rows"), 0)
	cols, colsOK := winDim(q.Get("cols"), 0)
	if rowsOK && colsOK && rows > 0 && cols > 0 {
		resizePTY(s.ptmx, cols, rows)
	}
	s.attach(conn)
}

// handleKillSession is POST /sessions/{id}/kill.
func handleKillSession(w http.ResponseWriter, r *http.Request) {
	s, ok := lookupSession(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	s.kill()
	writeJSON(w, http.StatusOK, map[string]string{"killed": s.ID})
}
