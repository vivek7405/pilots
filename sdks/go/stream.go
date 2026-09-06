package pilots

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// ExecStreamOptions are the exec stream's query parameters.
//
// Stdin is off by default, and deliberately so: a process holding an open
// stdin it never reads hangs, and the reference agent workload is exactly such
// a process.
type ExecStreamOptions struct {
	Dir   string
	Env   map[string]string
	User  string
	Stdin bool

	// TTY runs the command on a pseudo-terminal instead of three pipes, which
	// is what an interactive shell, tmux and vim need. It implies Stdin,
	// merges stderr into Stdout (a PTY has one device), and turns Stdin.Close
	// into an EOT character rather than a close. Resize becomes callable.
	TTY bool
	// Initial terminal window, 1..65535 each. Zero takes the default, 24 by
	// 80. Meaningful only with TTY.
	Rows uint16
	Cols uint16
}

// ExecStream is a running command.
//
// Stdout and Stderr are io.Pipe readers, so the reader goroutine blocks until
// a consumer reads. Read both concurrently or call Output, which drains both
// for you -- exactly the caveat os/exec makes about StdoutPipe. Under TTY a
// PTY merges the two, so everything arrives on Stdout and Stderr never
// produces a byte; it still has to be drained, which Output does.
type ExecStream struct {
	Stdout io.Reader
	Stderr io.Reader
	// Stdin is nil unless the stream was opened with Stdin or TTY. Close it to
	// send the stdin EOF frame.
	Stdin io.WriteCloser

	// The read ends of the pipes above, kept so Close can unblock a frame
	// loop parked writing output nobody is reading.
	stdoutR *io.PipeReader
	stderrR *io.PipeReader

	conn   *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}
	// ctx is the stream's own context, kept so Resize can write a control
	// message on the same connection the frame loop reads.
	ctx context.Context
	tty bool

	mu   sync.Mutex
	code int
	err  error
}

// ExecStream opens a websocket and streams a command's output frame by frame.
//
// The key travels in the Authorization header: Go can set handshake headers,
// so there is no reason to spend the subprotocol slot on it here.
func (m *Machines) ExecStream(ctx context.Context, id string, argv []string, opts ExecStreamOptions) (*ExecStream, error) {
	if len(argv) == 0 {
		return nil, errors.New("pilots: ExecStream needs at least one argv element")
	}
	target := execURL(m.c.baseURL, "/v1/machines/"+url.PathEscape(id)+"/exec/stream", argv, opts)

	// Detached from the caller's context on purpose: the stream outlives the
	// dial, and cancelling it is Close's job.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	conn, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + m.c.apiKey}},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	// A frame is one process write; the ceiling only has to be larger than
	// that, and a small one would drop a long compiler diagnostic.
	conn.SetReadLimit(4 << 20)

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	s := &ExecStream{
		Stdout: stdoutR, Stderr: stderrR,
		stdoutR: stdoutR, stderrR: stderrR,
		conn: conn, cancel: cancel, done: make(chan struct{}), code: -1,
		ctx: streamCtx,
	}
	if opts.Stdin || opts.TTY {
		s.Stdin = &stdinWriter{conn: conn, ctx: streamCtx}
	}
	s.tty = opts.TTY
	go s.read(streamCtx, stdoutW, stderrW)
	return s, nil
}

// execURL builds the exec-stream URL with the query names sprites uses.
// `path` is argv[0] repeated: hostd ignores it, but sprites clients send it.
func execURL(baseURL, path string, argv []string, opts ExecStreamOptions) string {
	u := strings.Replace(baseURL, "http", "ws", 1) + path
	q := url.Values{}
	for _, arg := range argv {
		q.Add("cmd", arg)
	}
	q.Set("path", argv[0])
	if opts.Dir != "" {
		q.Set("dir", opts.Dir)
	}
	for k, v := range opts.Env {
		q.Add("env", k+"="+v)
	}
	if opts.User != "" {
		q.Set("user", opts.User)
	}
	// Always present, never inferred: the default is the value most likely to
	// be wrong by omission. A tty forces it on rather than letting the pair
	// contradict itself: hostd answers tty=true&stdin=false with a 400.
	if opts.Stdin || opts.TTY {
		q.Set("stdin", "true")
	} else {
		q.Set("stdin", "false")
	}
	if opts.TTY {
		q.Set("tty", "true")
		if opts.Rows != 0 {
			q.Set("rows", strconv.FormatUint(uint64(opts.Rows), 10))
		}
		if opts.Cols != 0 {
			q.Set("cols", strconv.FormatUint(uint64(opts.Cols), 10))
		}
	}
	return u + "?" + q.Encode()
}

// read is the frame loop, the guest agent's writer in reverse.
func (s *ExecStream) read(ctx context.Context, stdout, stderr *io.PipeWriter) {
	defer close(s.done)

	finish := func(err error) {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
		// Ending both pipes is what lets a consumer's io.Copy return rather
		// than block forever on a stream that will never produce again.
		_ = stdout.CloseWithError(err)
		_ = stderr.CloseWithError(err)
	}

	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			s.mu.Lock()
			exited := s.code >= 0
			s.mu.Unlock()
			if exited {
				// The exit frame already arrived; this is the close that
				// follows it.
				finish(nil)
			} else {
				// A close with no exit frame is an error, never a silent zero:
				// nobody knows what the command did.
				finish(errors.New("pilots: stream closed before exit"))
			}
			return
		}

		if typ == websocket.MessageText {
			// hostd sends a text exit object alongside the binary frame. It is
			// a fallback, not a second source of truth: whichever arrives
			// first decides.
			var msg struct {
				Type     string `json:"type"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "exit" {
				s.mu.Lock()
				if s.code < 0 {
					s.code = msg.ExitCode
				}
				s.mu.Unlock()
				finish(nil)
				_ = s.conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			continue
		}

		if len(data) == 0 {
			continue
		}
		payload := data[1:]
		switch data[0] {
		case FrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				finish(err)
				return
			}
		case FrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				finish(err)
				return
			}
		case FrameExit:
			code := 0
			if len(payload) > 0 {
				code = int(payload[0])
			}
			s.mu.Lock()
			if s.code < 0 {
				s.code = code
			}
			s.mu.Unlock()
			// The pipes end before the code is readable, so a consumer that
			// saw Wait return has already seen every byte of output.
			finish(nil)
			_ = s.conn.Close(websocket.StatusNormalClosure, "")
			return
		default:
			// An id this version does not know is not a reason to fail: the
			// protocol is allowed to grow ids a newer agent sends.
		}
	}
}

// Resize changes the terminal window. It errors on a stream opened without TTY.
//
// The message is text rather than a binary frame on purpose: it is a control
// message, so it spends no byte id and an agent that does not implement it
// drops it the way it always dropped text frames.
func (s *ExecStream) Resize(cols, rows uint16) error {
	if !s.tty {
		return errors.New("pilots: this stream was opened without TTY")
	}
	msg, err := json.Marshal(struct {
		Type string `json:"type"`
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}{Type: "resize", Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	return s.conn.Write(s.ctx, websocket.MessageText, msg)
}

// Wait blocks until the command exits and returns its exit code.
//
// It returns an error when the stream closed without an exit frame. Read
// Stdout and Stderr concurrently, or call Output instead: an unread pipe
// blocks the frame loop, and Wait with it.
func (s *ExecStream) Wait() (int, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	return s.code, nil
}

// Output drains both streams and returns them with the exit code. The
// convenient form when the output fits in memory.
func (s *ExecStream) Output() (stdout, stderr []byte, code int, err error) {
	var wg sync.WaitGroup
	var outErr, errErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdout, outErr = io.ReadAll(s.Stdout)
	}()
	go func() {
		defer wg.Done()
		stderr, errErr = io.ReadAll(s.Stderr)
	}()
	wg.Wait()

	code, err = s.Wait()
	if err != nil {
		return stdout, stderr, 0, err
	}
	if outErr != nil {
		return stdout, stderr, code, outErr
	}
	return stdout, stderr, code, errErr
}

// Close ends the stream. The agent's context cancel kills the process.
//
// It closes the read ends of both pipes before waiting on the frame loop.
// Without that, a caller who never read Stdout would deadlock here: the loop
// would still be parked inside a pipe write that no reader will ever take, so
// closing the socket alone does not return it, and Close would block forever.
func (s *ExecStream) Close() error {
	// Before the socket, not after: the close handshake waits on the peer, and
	// a frame loop parked in a pipe write can neither read the peer's reply nor
	// drain, so closing the socket first would block here for the handshake's
	// full timeout and then wait on a goroutine that never returns.
	_ = s.stdoutR.CloseWithError(errStreamClosed)
	_ = s.stderrR.CloseWithError(errStreamClosed)
	err := s.conn.Close(websocket.StatusNormalClosure, "")
	s.cancel()
	<-s.done
	return err
}

// errStreamClosed is what an unread pipe reports once Close has abandoned it.
var errStreamClosed = errors.New("pilots: stream closed")

// stdinWriter frames what a caller writes as stdin chunks, and its Close as
// the stdin EOF frame. Under TTY the agent turns that frame into EOT on the
// terminal instead of closing an input, and the session stays open.
type stdinWriter struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (w *stdinWriter) Write(p []byte) (int, error) {
	frame := make([]byte, 0, len(p)+1)
	frame = append(frame, FrameStdin)
	frame = append(frame, p...)
	if err := w.conn.Write(w.ctx, websocket.MessageBinary, frame); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *stdinWriter) Close() error {
	return w.conn.Write(w.ctx, websocket.MessageBinary, []byte{FrameStdinEOF})
}
