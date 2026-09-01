package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

func TestMain(m *testing.M) {
	agentToken.Store("test-token")
	os.Exit(m.Run())
}

// testUser is the account the test process can actually exec as. In a real
// machine the agent runs as root and drops to the unprivileged guest user;
// in a test it is already unprivileged, so it targets itself.
func testUser() string {
	u, err := user.Current()
	if err != nil {
		return defaultGuestUser
	}
	return u.Username
}

func postExec(t *testing.T, body execRequest, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/exec", bytes.NewReader(raw))
	if auth {
		req.Header.Set("Authorization", "Bearer "+currentToken())
	}
	rec := httptest.NewRecorder()
	requireAuth(handleExec)(rec, req)
	return rec
}

func decodeExec(t *testing.T, rec *httptest.ResponseRecorder) execResponse {
	t.Helper()
	var resp execResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestExecReturnsStdoutAndExitCode(t *testing.T) {
	rec := postExec(t, execRequest{Cmd: "echo hello", User: testUser()}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	resp := decodeExec(t, rec)
	if strings.TrimSpace(resp.Stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", resp.Stdout)
	}
	if resp.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", resp.ExitCode)
	}
}

// A non-zero exit is a normal result, not a transport error: the HTTP call
// still succeeds and the code is carried in the body.
func TestExecPropagatesNonZeroExit(t *testing.T) {
	rec := postExec(t, execRequest{Cmd: "exit 42", User: testUser()}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 -- a failing command is not a transport error", rec.Code)
	}
	if got := decodeExec(t, rec).ExitCode; got != 42 {
		t.Errorf("exit code = %d, want 42", got)
	}
}

func TestExecCapturesStderrSeparately(t *testing.T) {
	resp := decodeExec(t, postExec(t, execRequest{Cmd: "echo out; echo err >&2", User: testUser()}, true))
	if strings.TrimSpace(resp.Stdout) != "out" {
		t.Errorf("stdout = %q, want out", resp.Stdout)
	}
	if strings.TrimSpace(resp.Stderr) != "err" {
		t.Errorf("stderr = %q, want err", resp.Stderr)
	}
}

// cwd and env are set on every call by the reference agent workload, in both
// the buffered and streaming paths.
func TestExecHonoursCwdAndEnv(t *testing.T) {
	resp := decodeExec(t, postExec(t, execRequest{
		Cmd: "pwd; echo $PILOT_TEST_VAR", Cwd: "/tmp", User: testUser(),
		Env: map[string]string{"PILOT_TEST_VAR": "present"},
	}, true))

	lines := strings.Fields(resp.Stdout)
	if len(lines) < 2 {
		t.Fatalf("stdout = %q, want a cwd line and an env line", resp.Stdout)
	}
	if lines[0] != "/tmp" {
		t.Errorf("cwd = %q, want /tmp", lines[0])
	}
	if lines[1] != "present" {
		t.Errorf("env var = %q, want present", lines[1])
	}
}

// The caller's env must beat the account defaults that prepareCommand seeds.
func TestExecEnvOverridesAccountDefaults(t *testing.T) {
	resp := decodeExec(t, postExec(t, execRequest{
		Cmd: "echo $HOME", User: testUser(),
		Env: map[string]string{"HOME": "/override"},
	}, true))
	if strings.TrimSpace(resp.Stdout) != "/override" {
		t.Errorf("HOME = %q, want the caller's /override", strings.TrimSpace(resp.Stdout))
	}
}

func TestExecRequiresAuth(t *testing.T) {
	if rec := postExec(t, execRequest{Cmd: "echo hi"}, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rec.Code)
	}
}

func TestExecRejectsEmptyCommand(t *testing.T) {
	if rec := postExec(t, execRequest{Cmd: "", User: testUser()}, true); rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rec.Code)
	}
}

// The predecessor logged a warning and ran as ROOT when the requested account
// was missing, silently turning an unprivileged exec into a privileged one.
// Fail closed instead: for untrusted code the uid is the isolation boundary.
func TestExecFailsClosedOnUnknownUser(t *testing.T) {
	rec := postExec(t, execRequest{Cmd: "id -u", User: "definitely-not-a-real-account"}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 -- an unknown user must not silently run as root", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "does not exist") {
		t.Errorf("error should name the cause, got %q", body)
	}
}

func TestAuthAcceptsQueryTokenForWebSockets(t *testing.T) {
	// Browsers cannot set headers on a WebSocket handshake, so the token is
	// also accepted as a query parameter.
	req := httptest.NewRequest("GET", "/exec/stream?token="+currentToken(), nil)
	if !authOK(req) {
		t.Error("query-parameter token was rejected")
	}
	if authOK(httptest.NewRequest("GET", "/exec/stream?token=wrong", nil)) {
		t.Error("a wrong query token was accepted")
	}
}

func TestAuthRejectsMalformedHeaders(t *testing.T) {
	for _, h := range []string{"", "Basic abc", "Bearer", "Bearer wrong", currentToken()} {
		req := httptest.NewRequest("GET", "/health", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		if authOK(req) {
			t.Errorf("Authorization %q was accepted", h)
		}
	}
}

// capture collects frames the way an SDK client would.
type capture struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *capture) Write(_ context.Context, _ websocket.MessageType, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, append([]byte(nil), p...))
	return nil
}

// The frame protocol is byte-compatible with sprites on purpose: byte 0 tags
// the stream, and an exit frame's payload byte is the code. Existing clients
// depend on this exact layout.
func TestFrameProtocolLayout(t *testing.T) {
	c := &capture{}
	fw := &frameWriter{conn: c, ctx: context.Background()}

	if err := fw.write(frameStdout, []byte("out")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := fw.write(frameStderr, []byte("err")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := fw.write(frameExit, []byte{7}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if len(c.frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(c.frames))
	}
	for i, want := range []struct {
		kind    byte
		payload string
	}{
		{frameStdout, "out"},
		{frameStderr, "err"},
	} {
		if c.frames[i][0] != want.kind {
			t.Errorf("frame %d: kind = %d, want %d", i, c.frames[i][0], want.kind)
		}
		if string(c.frames[i][1:]) != want.payload {
			t.Errorf("frame %d: payload = %q, want %q", i, c.frames[i][1:], want.payload)
		}
	}
	exit := c.frames[2]
	if exit[0] != frameExit || exit[1] != 7 {
		t.Errorf("exit frame = %v, want [%d 7]", exit, frameExit)
	}
}

// The pumps and the exit frame all write to one connection, and concurrent
// websocket writes are forbidden -- so the writer must serialise them.
func TestFrameWriterIsConcurrencySafe(t *testing.T) {
	c := &capture{}
	fw := &frameWriter{conn: c, ctx: context.Background()}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = fw.write(frameStdout, []byte("a")) }()
		go func() { defer wg.Done(); _ = fw.write(frameStderr, []byte("b")) }()
	}
	wg.Wait()

	if len(c.frames) != 100 {
		t.Errorf("got %d frames, want 100 -- writes were lost or interleaved", len(c.frames))
	}
	for i, f := range c.frames {
		if len(f) != 2 || (f[0] != frameStdout && f[0] != frameStderr) {
			t.Fatalf("frame %d is corrupt: %v", i, f)
		}
	}
}

func TestPumpFramesUntilEOF(t *testing.T) {
	c := &capture{}
	fw := &frameWriter{conn: c, ctx: context.Background()}

	done := make(chan struct{})
	go pump(fw, frameStdout, io.NopCloser(strings.NewReader("hello")), func() { close(done) })
	<-done

	if len(c.frames) != 1 || string(c.frames[0][1:]) != "hello" {
		t.Errorf("frames = %v, want one stdout frame carrying hello", c.frames)
	}
}

func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != 0 {
		t.Errorf("nil error -> %d, want 0", got)
	}
	err := exec.Command("/bin/bash", "-c", "exit 3").Run()
	if got := exitCodeOf(err); got != 3 {
		t.Errorf("exit 3 -> %d, want 3", got)
	}
	// A command that cannot start is reported as 127, the shell's convention,
	// so it is distinguishable from any code the command itself could return.
	if got := exitCodeOf(exec.Command("/nonexistent-binary").Run()); got != 127 {
		t.Errorf("unstartable command -> %d, want 127", got)
	}
}
