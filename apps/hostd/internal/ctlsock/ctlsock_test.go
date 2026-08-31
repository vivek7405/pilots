package ctlsock

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// start runs a server over a temp socket.
func start(t *testing.T, h Handler) string {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "ctl.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go Serve(ln, h)
	return sock
}

// The payload is length-prefixed, so a reply carrying binary data has to come
// back byte for byte. A bitmap that loses its tail restores a machine whose
// disk is missing whatever those blocks held.
func TestRequestRoundTripsABinaryPayload(t *testing.T) {
	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(i % 251)
	}
	sock := start(t, func(string) ([]byte, error) { return want, nil })

	got, err := Request(sock, "give")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the payload came back altered")
	}
}

// A bare acknowledgement carries no payload, and the caller must not block
// waiting for one.
func TestRequestHandlesAnEmptyPayload(t *testing.T) {
	sock := start(t, func(string) ([]byte, error) { return nil, nil })

	got, err := Request(sock, "ack")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bytes for a bare acknowledgement", len(got))
	}
}

// A handler's failure has to reach the caller as an error. Silently returning
// no payload would look like an empty bitmap -- a machine that wrote nothing --
// and the checkpoint would quietly discard its disk.
func TestRequestSurfacesAHandlerError(t *testing.T) {
	sock := start(t, func(string) ([]byte, error) {
		return nil, errors.New("the cache could not be synced")
	})

	if _, err := Request(sock, "dirty"); err == nil {
		t.Fatal("a failing handler reported success")
	}
}

// An error containing newlines must not be able to forge a status line.
func TestHandlerErrorsCannotForgeAStatusLine(t *testing.T) {
	sock := start(t, func(string) ([]byte, error) {
		return nil, errors.New("first line\nOK 4\nEVIL")
	})

	_, err := Request(sock, "dirty")
	if err == nil {
		t.Fatal("expected an error")
	}
	if bytes.Contains([]byte(err.Error()), []byte("\n")) {
		t.Errorf("the error carried a newline into the status line: %q", err)
	}
}

// The command reaches the handler verbatim.
func TestHandlerSeesTheCommand(t *testing.T) {
	var seen atomic.Value
	sock := start(t, func(cmd string) ([]byte, error) {
		seen.Store(cmd)
		return nil, nil
	})

	if _, err := Request(sock, "prefault"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got := seen.Load(); got != "prefault" {
		t.Errorf("handler saw %q, want \"prefault\"", got)
	}
}

// A handler serves one request per connection but many over its life -- one
// per checkpoint -- so the listener must survive each.
func TestServeHandlesRepeatedRequests(t *testing.T) {
	var n atomic.Int32
	sock := start(t, func(string) ([]byte, error) {
		return fmt.Appendf(nil, "%d", n.Add(1)), nil
	})

	for i := 1; i <= 5; i++ {
		got, err := Request(sock, "count")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if string(got) != fmt.Sprint(i) {
			t.Errorf("request %d returned %q", i, got)
		}
	}
}

// A socket left behind by a handler that was SIGKILLed must not stop the next
// one binding, or a machine that died badly could never start again.
func TestListenClearsAStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl.sock")

	first, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Close it the way a SIGKILL does: the file stays behind.
	if ul, ok := first.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	first.Close()

	second, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	second.Close()
}

// Dialling a socket nobody is listening on is an error, not a hang.
func TestRequestFailsWhenNothingIsListening(t *testing.T) {
	if _, err := Request(filepath.Join(t.TempDir(), "absent.sock"), "dirty"); err == nil {
		t.Error("dialling an absent socket reported success")
	}
}
