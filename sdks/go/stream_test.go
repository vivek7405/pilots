package pilots

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsServer accepts one upgrade, asserts the bearer header, and hands the
// connection to the body.
func wsServer(t *testing.T, body func(t *testing.T, conn *websocket.Conn, r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q, want the bearer header", got)
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		body(t, conn, r)
	}))
	t.Cleanup(srv.Close)
	return New("k", WithBaseURL(srv.URL))
}

func frame(id byte, payload string) []byte {
	return append([]byte{id}, payload...)
}

func TestExecStreamDecodesFrames(t *testing.T) {
	// The three orders that can reach a client: the binary exit alone, the
	// text object after it, and the text object alone.
	cases := map[string]func(ctx context.Context, conn *websocket.Conn){
		"binary exit only": func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameExit, "\x07"))
		},
		"binary exit then text": func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameExit, "\x07"))
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"exit","exit_code":7}`))
		},
		"text only": func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"exit","exit_code":7}`))
		},
	}

	for name, writeExit := range cases {
		t.Run(name, func(t *testing.T) {
			c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
				ctx := context.Background()
				_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameStdout, "a"))
				_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameStderr, "b"))
				// Neither of these may reach the consumer.
				_ = conn.Write(ctx, websocket.MessageBinary, nil)
				_ = conn.Write(ctx, websocket.MessageBinary, frame(9, "from a newer agent"))
				writeExit(ctx, conn)
				time.Sleep(50 * time.Millisecond)
			})

			s, err := c.Machines.ExecStream(context.Background(), "m-1",
				[]string{"bash", "-c", "true"}, ExecStreamOptions{})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			stdout, stderr, code, err := s.Output()
			if err != nil {
				t.Fatalf("output: %v", err)
			}
			if string(stdout) != "a" || string(stderr) != "b" || code != 7 {
				t.Errorf("stdout=%q stderr=%q code=%d", stdout, stderr, code)
			}
		})
	}
}

func TestExecStreamDialURL(t *testing.T) {
	got := make(chan url.Values, 1)
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		got <- r.URL.Query()
		_ = conn.Write(context.Background(), websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1",
		[]string{"bash", "-c", "echo hi"},
		ExecStreamOptions{Dir: "/home/sprite/app", Env: map[string]string{"A": "1"}, User: "sprite"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer s.Close()

	q := <-got
	if want := []string{"bash", "-c", "echo hi"}; len(q["cmd"]) != 3 ||
		q["cmd"][0] != want[0] || q["cmd"][1] != want[1] || q["cmd"][2] != want[2] {
		t.Errorf("cmd = %v, want %v in order", q["cmd"], want)
	}
	if q.Get("path") != "bash" || q.Get("dir") != "/home/sprite/app" ||
		q.Get("user") != "sprite" || q.Get("stdin") != "false" {
		t.Errorf("query = %v", q)
	}
	if len(q["env"]) != 1 || q["env"][0] != "A=1" {
		t.Errorf("env = %v", q["env"])
	}
}

func TestExecStreamStdinFrames(t *testing.T) {
	received := make(chan []byte, 2)
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		if r.URL.Query().Get("stdin") != "true" {
			t.Errorf("stdin = %q", r.URL.Query().Get("stdin"))
		}
		ctx := context.Background()
		for range 2 {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			received <- data
		}
		_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1",
		[]string{"cat"}, ExecStreamOptions{Stdin: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if s.Stdin == nil {
		t.Fatal("Stdin is nil on a stream opened with Stdin: true")
	}
	if _, err := s.Stdin.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Stdin.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	chunk, eof := <-received, <-received
	if len(chunk) != 2 || chunk[0] != FrameStdin || chunk[1] != 'x' {
		t.Errorf("stdin chunk = %v, want [0 120]", chunk)
	}
	if len(eof) != 1 || eof[0] != FrameStdinEOF {
		t.Errorf("stdin eof = %v, want [4]", eof)
	}
	if _, err := s.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestExecStreamWithoutStdinHasNoWriter(t *testing.T) {
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		_ = conn.Write(context.Background(), websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})
	s, err := c.Machines.ExecStream(context.Background(), "m-1", []string{"true"}, ExecStreamOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer s.Close()
	if s.Stdin != nil {
		t.Error("Stdin is non-nil on a stream that did not ask for it")
	}
}

func TestCloseWithoutExitIsAnError(t *testing.T) {
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		_ = conn.Write(context.Background(), websocket.MessageBinary, frame(FrameStdout, "partial"))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1", []string{"true"}, ExecStreamOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// A dropped socket means nobody knows what the command did, so this must
	// never read as exit code 0.
	if _, _, _, err := s.Output(); err == nil {
		t.Fatal("a close with no exit frame was reported as a clean exit")
	}
}

func TestOutputIsCompleteWhenWaitReturns(t *testing.T) {
	// 64 KiB of stdout, written before the exit frame: the agent drains both
	// pumps first, so every byte has to be readable once Wait returns.
	big := make([]byte, 64<<10)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageBinary, append([]byte{FrameStdout}, big...))
		_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1", []string{"true"}, ExecStreamOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	stdout, _, code, err := s.Output()
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if code != 0 || len(stdout) != len(big) {
		t.Errorf("code=%d stdout=%d bytes, want 0 and %d", code, len(stdout), len(big))
	}
}

func TestExecStreamNeedsArgv(t *testing.T) {
	if _, err := New("k").Machines.ExecStream(context.Background(), "m-1", nil, ExecStreamOptions{}); err == nil {
		t.Fatal("an empty argv was accepted")
	}
}

func TestCloseReturnsWithUnreadOutput(t *testing.T) {
	// The frame loop parks inside a pipe write until someone reads it. Close
	// has to end the stream anyway: a caller that only wanted the exit, or that
	// is abandoning the command, never reads Stdout, and Close blocking on that
	// goroutine forever is a deadlock in the caller's process.
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameStdout, "output nobody reads"))
		// Parked in a read, so the client's close handshake is answered.
		_, _, _ = conn.Read(ctx)
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1", []string{"sh"}, ExecStreamOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the frame arrive and block the pipe

	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a frame loop parked writing unread output")
	}
}

// TTY writes the terminal query names and forces stdin on.
//
// Never sent as stdin=false: hostd answers that pair with a 400, so the
// contradiction is settled in the SDK rather than at the wire.
func TestExecStreamTTYQuery(t *testing.T) {
	got := execURL("http://h", "/v1/machines/m-1/exec/stream", []string{"bash", "-l"},
		ExecStreamOptions{TTY: true, Rows: 40, Cols: 120}, "")
	q, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	for key, want := range map[string]string{"tty": "true", "rows": "40", "cols": "120", "stdin": "true"} {
		if have := q.Query().Get(key); have != want {
			t.Errorf("%s = %q, want %q", key, have, want)
		}
	}

	// Without TTY none of the three appears, so a plain exec's URL is
	// byte-for-byte what it always was.
	plain, err := url.Parse(execURL("http://h", "/p", []string{"true"}, ExecStreamOptions{}, ""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, key := range []string{"tty", "rows", "cols"} {
		if have := plain.Query().Get(key); have != "" {
			t.Errorf("%s = %q on a non-tty stream, want absent", key, have)
		}
	}
}

// A tty stream opens Stdin without being asked, and Resize is a TEXT control
// message rather than a frame: it spends no byte id.
func TestExecStreamTTYResize(t *testing.T) {
	received := make(chan string, 1)
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		ctx := context.Background()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			t.Errorf("resize arrived as a %v message, want text", typ)
		}
		received <- string(data)
		_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1",
		[]string{"bash", "-l"}, ExecStreamOptions{TTY: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Stdin: true was never passed. A terminal implies it.
	if s.Stdin == nil {
		t.Fatal("Stdin is nil on a TTY stream")
	}
	if err := s.Resize(100, 30); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if got := <-received; got != `{"type":"resize","cols":100,"rows":30}` {
		t.Errorf("resize message = %s", got)
	}
	if _, err := s.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestResizeWithoutTTYErrors(t *testing.T) {
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		_ = conn.Write(context.Background(), websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})
	s, err := c.Machines.ExecStream(context.Background(), "m-1", []string{"true"}, ExecStreamOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer s.Close()
	if err := s.Resize(100, 30); err == nil {
		t.Error("Resize was accepted on a stream opened without TTY")
	}
}

// WithOrg narrows the exec stream too.
//
// Every other call goes through `request`, which applies the narrowing; this
// URL is built by hand, so an admin client acting as one org used to open a
// shell on another org's machine while WithOrg's doc promised otherwise.
func TestExecStreamCarriesTheOrgNarrowing(t *testing.T) {
	got := execURL("http://h", "/v1/machines/m-1/exec/stream", []string{"true"}, ExecStreamOptions{}, "org_a")
	q, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if have := q.Query().Get("org"); have != "org_a" {
		t.Errorf("org = %q, want org_a", have)
	}
	// A client that is not acting as an org sends no org at all, so a
	// tenant key's URL is unchanged.
	plain, _ := url.Parse(execURL("http://h", "/p", []string{"true"}, ExecStreamOptions{}, ""))
	if have := plain.Query().Get("org"); have != "" {
		t.Errorf("org = %q on an unnarrowed client, want absent", have)
	}
}
