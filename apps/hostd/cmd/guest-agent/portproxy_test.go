package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A WebSocket has to survive the last hop.
//
// The router's cross-host hop is covered by the router's own tests, but that
// only gets a request to the host that owns the machine. The final hop is this
// one: the guest agent forwards to 127.0.0.1:<port> inside the guest, and if
// the upgrade dies here every live feature of every framework dies with it --
// Rails' Action Cable, which is what Turbo Streams broadcasts over, Phoenix
// LiveView, Django Channels, and a Vite dev server's HMR socket. The failure
// is quiet: the page loads, and nothing ever updates.
//
// proxyToLocalPort installs a custom Director, and a Director that rewrote the
// wrong headers would strip Connection/Upgrade and turn the handshake into a
// plain 200. Nothing asserted otherwise until this test.
func TestAWebsocketReachesTheApplicationThroughThePortProxy(t *testing.T) {
	// The application inside the guest: it echoes, so the test can prove
	// frames move in BOTH directions rather than only that the handshake
	// returned 101.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		// The proxy must not leak its own routing header into the app.
		if r.Header.Get(headerProxyPort) != "" {
			http.Error(w, headerProxyPort+" was reflected onward", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			typ, msg, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), typ, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}))
	defer app.Close()

	_, appPort, err := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	// The agent, with nothing behind the proxy: a request carrying the
	// header never reaches the inner handler.
	agent := httptest.NewServer(withPortProxy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the port proxy did not claim the request", http.StatusTeapot)
	})))
	defer agent.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(agent.URL, "http")+"/cable",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProxyPort: []string{appPort}}})
	if err != nil {
		code := 0
		if res != nil {
			code = res.StatusCode
		}
		t.Fatalf("the upgrade did not survive the guest agent (status %d): %v", code, err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write through the proxy: %v", err)
	}
	typ, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read back through the proxy: %v", err)
	}
	if typ != websocket.MessageText || string(got) != "echo:hello" {
		t.Fatalf("got %s %q, want text \"echo:hello\"", typ, got)
	}

	// A second frame, because a proxy that copied one buffer and stopped
	// would pass everything above.
	if err := conn.Write(ctx, websocket.MessageText, []byte("again")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if _, got, err = conn.Read(ctx); err != nil || string(got) != "echo:again" {
		t.Fatalf("second frame: got %q err %v", got, err)
	}
}

// The same hop for Server-Sent Events, which is the other half of how a live
// page updates -- Turbo Streams falls back to SSE, and a buffering proxy
// breaks it in exactly the way the WebSocket case does not reveal: the
// handshake succeeds and the first event never arrives.
func TestServerSentEventsAreNotBufferedByThePortProxy(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server cannot flush")
			return
		}
		for i := 0; i < 2; i++ {
			_, _ = w.Write([]byte("data: tick\n\n"))
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer app.Close()

	_, appPort, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
	agent := httptest.NewServer(withPortProxy(http.NotFoundHandler()))
	defer agent.Close()

	req, _ := http.NewRequest("GET", agent.URL+"/events", nil)
	req.Header.Set(headerProxyPort, appPort)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// The first event has to arrive before the handler has finished. A
	// proxy that buffered the whole response would block this read until
	// both ticks were written, so the deadline is what proves it streams.
	type read struct {
		n   int
		err error
	}
	done := make(chan read, 1)
	go func() {
		buf := make([]byte, 12)
		n, err := res.Body.Read(buf)
		done <- read{n, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.n == 0 {
			t.Fatalf("first event: n=%d err=%v", r.n, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event never arrived: the proxy is buffering the stream")
	}
}

// The 502 path, so the streaming tests above cannot pass by accident against
// a proxy that answers everything.
func TestThePortProxyAnswers502WhenNothingIsListening(t *testing.T) {
	agent := httptest.NewServer(withPortProxy(http.NotFoundHandler()))
	defer agent.Close()

	// Port 1 is reserved and nothing binds it.
	req, _ := http.NewRequest("GET", agent.URL+"/", nil)
	req.Header.Set(headerProxyPort, "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
}
