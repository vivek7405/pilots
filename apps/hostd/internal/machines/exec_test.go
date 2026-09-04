package machines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// streamManager is a Manager with a store holding one machine, which is what
// the transport half needs: a derived agent token and a row to touch.
func streamManager(t *testing.T) (*Manager, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.PutMachine(context.Background(), &state.Machine{
		ID: "m_1", Name: "webapp", HostID: "host-a", State: StateRunning,
		LastActivity: time.Now().Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	m := testManager()
	m.opts.Store = st
	m.opts.AgentTokenSecret = "s"
	m.opts.StateRoot = t.TempDir()
	return m, st
}

// What the guest saw, recorded by a fake agent.
type seenRequest struct {
	path, rawQuery, auth, subprotocol string
}

// fakeGuest accepts one exec stream and holds it open until release is closed.
func fakeGuest(t *testing.T, seen chan<- seenRequest, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- seenRequest{
			path:        r.URL.Path,
			rawQuery:    r.URL.RawQuery,
			auth:        r.Header.Get("Authorization"),
			subprotocol: r.Header.Get("Sec-WebSocket-Protocol"),
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		<-release
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The proxy is the whole of hostd's exec streaming: the query crosses
// verbatim, the API key is replaced by the machine's own agent token, and the
// offered subprotocol comes back on the 101 -- a client that offered one and
// is answered with none fails the connection.
func TestExecStreamProxiesToTheGuest(t *testing.T) {
	m, st := streamManager(t)
	seen := make(chan seenRequest, 1)
	release := make(chan struct{})
	guest := fakeGuest(t, seen, release)
	addr := strings.TrimPrefix(guest.URL, "http://")

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.execStreamAt(w, r, "m_1", addr)
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(front.URL, "http")+"/v1/machines/m_1/exec/stream?cmd=ls&stdin=false",
		&websocket.DialOptions{Subprotocols: []string{"authorization.bearer.k"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", res.StatusCode)
	}
	if got := conn.Subprotocol(); got != "authorization.bearer.k" {
		t.Errorf("the 101 chose subprotocol %q, want the offered entry echoed", got)
	}

	got := <-seen
	if got.path != "/exec/stream" {
		t.Errorf("the guest saw path %q", got.path)
	}
	if got.rawQuery != "cmd=ls&stdin=false" {
		t.Errorf("the guest saw query %q, want it verbatim", got.rawQuery)
	}
	if want := "Bearer " + m.token("m_1"); got.auth != want {
		t.Errorf("the guest saw Authorization %q, want %q", got.auth, want)
	}
	if got.subprotocol != "" {
		t.Errorf("the API key reached the guest as %q", got.subprotocol)
	}

	// The stream is in flight for the whole life of the socket, so the idle
	// monitor cannot suspend the machine mid-command.
	if n := m.flight.count("m_1"); n != 1 {
		t.Errorf("in flight = %d while the socket is open, want 1", n)
	}

	// Read until the guest's close arrives, so the proxy's copy finishes and
	// ServeHTTP returns -- which is when the stream stops counting.
	close(release)
	if _, _, err := conn.Read(ctx); err == nil {
		t.Error("the socket stayed open after the guest closed it")
	}
	waitFor(t, func() bool { return m.flight.count("m_1") == 0 })

	row, err := st.GetMachine(context.Background(), "m_1")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(time.Unix(row.LastActivity, 0)) > time.Minute {
		t.Error("the stream did not touch the row; a long stream would look idle")
	}
}

// The echo covers offers that carry no key.
//
// A client may authenticate with the Authorization header and still offer a
// subprotocol of its own. Echoing only the `authorization.bearer.` entry left
// that client answered with none, which the WHATWG handshake algorithm treats
// as a failed connection: authenticated, upgraded, and immediately dropped.
func TestExecStreamEchoesASubprotocolThatCarriesNoKey(t *testing.T) {
	m, _ := streamManager(t)
	seen := make(chan seenRequest, 1)
	release := make(chan struct{})
	guest := fakeGuest(t, seen, release)
	addr := strings.TrimPrefix(guest.URL, "http://")

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.execStreamAt(w, r, "m_1", addr)
	}))
	defer front.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(front.URL, "http")+"/v1/machines/m_1/exec/stream?cmd=ls",
		&websocket.DialOptions{
			Subprotocols: []string{"pilots.v1"},
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer k"}},
		})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", res.StatusCode)
	}
	if got := conn.Subprotocol(); got != "pilots.v1" {
		t.Errorf("the 101 chose %q, want the first offered entry echoed", got)
	}
	if got := <-seen; got.subprotocol != "" {
		t.Errorf("the guest was asked to negotiate %q", got.subprotocol)
	}
}

// A guest that cannot be reached is a JSON 502, not a hijacked socket and not
// an empty body.
func TestExecStreamAnswers502WhenTheGuestIsUnreachable(t *testing.T) {
	m, _ := streamManager(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/machines/m_1/exec/stream?cmd=ls", nil)
	// 127.0.0.1:1 is reserved and never listening.
	m.execStreamAt(rec, req, "m_1", "127.0.0.1:1")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "machine unreachable") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// LogsFrom is what a follow polls. It reads from an offset, answers nothing
// when there is nothing new, and ErrNotFound once the row is deleted -- which
// is the only thing that ends a follow besides the client leaving.
func TestLogsFromReadsAnOffsetAndEndsOnDestroy(t *testing.T) {
	m, st := streamManager(t)
	ctx := context.Background()

	if got, err := m.LogsFrom(ctx, "m_1", 0); err != nil || got != nil {
		t.Errorf("with no file: %q, %v; want nil, nil", got, err)
	}

	dir := m.stateDir("m_1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lifecycle.log"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		offset int64
		want   string
	}{{0, "abc"}, {2, "c"}, {3, ""}} {
		got, err := m.LogsFrom(ctx, "m_1", tc.offset)
		if err != nil {
			t.Fatalf("offset %d: %v", tc.offset, err)
		}
		if string(got) != tc.want {
			t.Errorf("offset %d = %q, want %q", tc.offset, got, tc.want)
		}
	}

	if err := st.DeleteMachine(ctx, "m_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LogsFrom(ctx, "m_1", 0); err == nil {
		t.Error("a destroyed machine's follow never ends")
	}
}

// waitFor polls a condition until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
