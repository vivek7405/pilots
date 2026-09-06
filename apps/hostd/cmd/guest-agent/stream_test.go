package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialStream opens a real exec stream against the real handler.
//
// A live socket rather than the frameWriter fake in exec_test.go: what is
// under test here is the READ side, which only exists once a client can send
// frames the handler's goroutine has to classify.
func dialStream(t *testing.T, query string) (*websocket.Conn, context.Context) {
	t.Helper()
	srv := httptest.NewServer(requireAuth(handleExecStream))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") +
		"/exec/stream?token=" + currentToken() + "&user=" + testUser() + "&" + query
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn, ctx
}

// readFrame returns the next message's type and payload.
func readFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, data
}

// readBinary returns the next BINARY frame, skipping nothing: a text frame
// arriving where a binary one is expected is the ordering bug this guards.
func readBinary(t *testing.T, ctx context.Context, conn *websocket.Conn) (byte, []byte) {
	t.Helper()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("got a %v frame %q, want binary", typ, data)
	}
	if len(data) == 0 {
		t.Fatal("an empty binary frame")
	}
	return data[0], data[1:]
}

// A client's stdin arrives as frame 0, not as raw bytes. Writing the frame
// whole is what the predecessor did, and it delivered the id byte to the
// process: `cat` would echo "\x00hello", not "hello".
func TestStdinFramesReachTheProcess(t *testing.T) {
	conn, ctx := dialStream(t, "stdin=true&cmd=cat")

	// Neither of these is stdin: an id this agent does not know, and a text
	// control message. Both must be dropped rather than written through.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{9, 'x'}); err != nil {
		t.Fatalf("write unknown frame: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize"}`)); err != nil {
		t.Fatalf("write text frame: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{frameStdin}, "hello\n"...)); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}

	kind, payload := readBinary(t, ctx, conn)
	if kind != frameStdout || string(payload) != "hello\n" {
		t.Fatalf("frame %d payload %q, want a stdout frame carrying hello", kind, payload)
	}

	// Frame 4 closes stdin, which is the only thing that ends `cat`.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{frameStdinEOF}); err != nil {
		t.Fatalf("write eof frame: %v", err)
	}

	typ, data := readFrame(t, ctx, conn)
	if typ != websocket.MessageText {
		t.Fatalf("got a %v frame %q, want the text verdict first", typ, data)
	}
	var verdict struct {
		Type     string `json:"type"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &verdict); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if verdict.Type != "exit" || verdict.ExitCode != 0 {
		t.Fatalf("text verdict = %+v, want exit 0", verdict)
	}

	kind, payload = readBinary(t, ctx, conn)
	if kind != frameExit || len(payload) != 1 || payload[0] != 0 {
		t.Fatalf("frame %d payload %v, want exit 0", kind, payload)
	}
}

// The text verdict comes FIRST, and the binary frame follows it.
//
// Both SDKs act on whichever verdict arrives first: they close the socket and
// return. A text frame written after the binary one is therefore a frame
// nothing can receive, which is what it was for a while -- documented as the
// untruncated fallback and structurally unable to fire.
func TestTheTextExitVerdictPrecedesTheBinaryOne(t *testing.T) {
	conn, ctx := dialStream(t, "stdin=false&cmd=sh&cmd=-c&cmd=exit+3")

	typ, data := readFrame(t, ctx, conn)
	if typ != websocket.MessageText || string(data) != `{"type":"exit","exit_code":3}` {
		t.Fatalf("got %v %q, want the text exit verdict for 3 first", typ, data)
	}
	kind, payload := readBinary(t, ctx, conn)
	if kind != frameExit || len(payload) != 1 || payload[0] != 3 {
		t.Fatalf("frame %d payload %v, want exit 3", kind, payload)
	}
}

// The untruncated code is what the ordering buys.
//
// A command killed by a signal has an ExitCode of -1, and byte(-1) is 255 --
// indistinguishable from a command that genuinely exited 255. The text verdict
// carries -1, and because it arrives first that is the code an SDK reports.
func TestASignalDeathIsUntruncatedInTheTextVerdict(t *testing.T) {
	conn, ctx := dialStream(t, "stdin=false&cmd=sh&cmd=-c&cmd=kill+-9+%24%24")

	typ, data := readFrame(t, ctx, conn)
	if typ != websocket.MessageText || string(data) != `{"type":"exit","exit_code":-1}` {
		t.Fatalf("got %v %q, want the text verdict carrying -1", typ, data)
	}
	kind, payload := readBinary(t, ctx, conn)
	if kind != frameExit || len(payload) != 1 || payload[0] != 255 {
		t.Fatalf("frame %d payload %v, want the byte frame truncating to 255", kind, payload)
	}
}

// stdin=false means nothing is read from the socket at all, so a 0 frame sent
// anyway is ignored rather than an error: the command's stdin stays the null
// device and it sees EOF immediately.
func TestAStdinFrameIsIgnoredWhenStdinIsOff(t *testing.T) {
	conn, ctx := dialStream(t, "stdin=false&cmd=sh&cmd=-c&cmd=read+x%3B+echo+got%3A%24x")

	if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{frameStdin}, "abc\n"...)); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}

	var out strings.Builder
	for {
		typ, data := readFrame(t, ctx, conn)
		if typ == websocket.MessageText {
			continue // the text verdict, which now precedes the binary one
		}
		if len(data) == 0 {
			t.Fatal("an empty binary frame")
		}
		if data[0] == frameExit {
			break
		}
		if data[0] == frameStdout {
			out.WriteString(string(data[1:]))
		}
	}
	if got := strings.TrimSpace(out.String()); got != "got:" {
		t.Fatalf("output %q, want got: -- the stdin frame was delivered", got)
	}
}

// drainStream reads frames until the exit verdict and returns what each output
// stream carried. Under a tty the stderr half must stay empty: a PTY merges
// the two, so a frame 2 would mean the pipe path ran when the terminal one was
// asked for.
func drainStream(t *testing.T, ctx context.Context, conn *websocket.Conn) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut strings.Builder
	code = -999
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageText {
			var verdict struct {
				Type     string `json:"type"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(data, &verdict) == nil && verdict.Type == "exit" {
				code = verdict.ExitCode
			}
			continue
		}
		if len(data) == 0 {
			t.Fatal("an empty binary frame")
		}
		switch data[0] {
		case frameStdout:
			out.WriteString(string(data[1:]))
		case frameStderr:
			errOut.WriteString(string(data[1:]))
		case frameExit:
			return out.String(), errOut.String(), code
		}
	}
}

// tty=true puts the command on a real terminal at the size that was asked for.
//
// `stty size` reads the window from the kernel, so it can only answer when
// there IS a terminal. Counterfactual: drop the tty branch and the command
// runs on pipes, where stty fails with "Inappropriate ioctl for device".
func TestTTYRunsTheCommandOnAPTYAtTheRequestedSize(t *testing.T) {
	conn, ctx := dialStream(t, "tty=true&rows=30&cols=100&cmd=sh&cmd=-c&cmd=stty+size")

	stdout, stderr, code := drainStream(t, ctx, conn)
	if !strings.Contains(stdout, "30 100") {
		t.Fatalf("stdout %q, want the 30 by 100 window", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr %q, want nothing: a PTY merges the streams onto frame 1", stderr)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}

// A resize control message reaches the terminal.
//
// The command waits for a line before reading the size, so the resize is
// guaranteed to have landed by the time stty runs. Counterfactual: drop the
// text branch in the read loop and the window stays 30 by 100.
func TestTTYHonoursAResizeMessage(t *testing.T) {
	conn, ctx := dialStream(t, "tty=true&rows=30&cols=100&cmd=sh&cmd=-c&cmd=read+x%3B+stty+size")

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{frameStdin}, "go\n"...)); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}

	stdout, _, code := drainStream(t, ctx, conn)
	if !strings.Contains(stdout, "40 120") {
		t.Fatalf("stdout %q, want the resized 40 by 120 window", stdout)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}

// A tty implies stdin even when stdin=true was never passed, and frame 4 is
// EOT rather than a close: `cat` ends on the EOT the line discipline delivers,
// and the session was never torn down to make that happen.
func TestTTYReadsStdinWithoutTheStdinFlagAndEndsOnEOT(t *testing.T) {
	conn, ctx := dialStream(t, "tty=true&cmd=cat")

	if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{frameStdin}, "hello\n"...)); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}
	// The terminal echoes what was typed and cat writes it back, so the read
	// below is looking for the line, not counting occurrences of it.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{frameStdinEOF}); err != nil {
		t.Fatalf("write eof frame: %v", err)
	}

	stdout, stderr, code := drainStream(t, ctx, conn)
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("stdout %q, want the typed line", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr %q, want nothing under a tty", stderr)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}

// A window size that is not 1..65535 is refused at the handshake rather than
// clamped, so a client that computed it wrongly learns immediately.
func TestABadWindowSizeClosesTheStream(t *testing.T) {
	for _, query := range []string{"tty=true&rows=0&cmd=cat", "tty=true&cols=abc&cmd=cat"} {
		conn, ctx := dialStream(t, query)
		_, _, err := conn.Read(ctx)
		status := websocket.CloseStatus(err)
		if status != websocket.StatusPolicyViolation {
			t.Fatalf("%s: close status %v (err %v), want 1008", query, status, err)
		}
	}
}

// trackedPIDs is the set of processes the handlers currently own. A handler
// untracks its own on the way out, so this is also how a test sees that one
// returned.
func trackedPIDs() map[int]bool {
	out := map[int]bool{}
	ownedPIDs.Range(func(key, _ any) bool {
		if pid, ok := key.(int); ok {
			out[pid] = true
		}
		return true
	})
	return out
}

// A client that goes away takes its terminal with it.
//
// A pipe stream ends itself: the deferred stdin close delivers EOF and the
// command exits. A terminal has no such end, and a shell waiting for input
// writes nothing, so without an explicit cancel the output pump parks on the
// PTY master forever -- and with it the handler, the connection and the shell,
// one set per closed browser tab.
//
// Counterfactual: drop the cancel in the read loop and this test fails on the
// poll below, because the handler never returns and never untracks its PID.
func TestClosingATTYStreamEndsTheShell(t *testing.T) {
	before := trackedPIDs()
	conn, _ := dialStream(t, "tty=true&cmd=sh&cmd=-c&cmd=read+x")

	var pid int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		for candidate := range trackedPIDs() {
			if !before[candidate] {
				pid = candidate
			}
		}
		if pid != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the shell never started")
	}

	conn.CloseNow()

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if !trackedPIDs()[pid] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the handler never returned: the shell outlived the client that asked for it")
}
