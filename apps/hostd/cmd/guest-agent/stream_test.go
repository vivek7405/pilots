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

	kind, payload = readBinary(t, ctx, conn)
	if kind != frameExit || len(payload) != 1 || payload[0] != 0 {
		t.Fatalf("frame %d payload %v, want exit 0", kind, payload)
	}

	typ, data := readFrame(t, ctx, conn)
	if typ != websocket.MessageText {
		t.Fatalf("got a %v frame after the exit, want the text verdict", typ)
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
}

// The text verdict carries the real code, and the binary one truncates to a
// byte. Both SDKs take whichever arrives first, so the two must agree.
func TestTheTextExitFrameFollowsTheBinaryOne(t *testing.T) {
	conn, ctx := dialStream(t, "stdin=false&cmd=sh&cmd=-c&cmd=exit+3")

	kind, payload := readBinary(t, ctx, conn)
	if kind != frameExit || len(payload) != 1 || payload[0] != 3 {
		t.Fatalf("frame %d payload %v, want exit 3", kind, payload)
	}
	typ, data := readFrame(t, ctx, conn)
	if typ != websocket.MessageText || string(data) != `{"type":"exit","exit_code":3}` {
		t.Fatalf("got %v %q, want the text exit verdict for 3", typ, data)
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
		kind, payload := readBinary(t, ctx, conn)
		if kind == frameExit {
			break
		}
		if kind == frameStdout {
			out.WriteString(string(payload))
		}
	}
	if got := strings.TrimSpace(out.String()); got != "got:" {
		t.Fatalf("output %q, want got: -- the stdin frame was delivered", got)
	}
}
