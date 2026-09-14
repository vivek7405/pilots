package fc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

// fakeVMStateAPI answers PATCH /vm on a unix socket and records each state.
func fakeVMStateAPI(t *testing.T) (string, func() []string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var states []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ State string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		states = append(states, body.State)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), states...)
	}
}

// The operation run while paused is exactly what exhausts a context. The
// resume must still be sent, or the guest stays frozen for good.
func TestWhilePausedResumesAfterTheContextIsDone(t *testing.T) {
	sock, states := fakeVMStateAPI(t)
	m := &Machine{ID: "m_1", Client: NewClient(sock)}

	ctx, cancel := context.WithCancel(context.Background())
	err := m.WhilePaused(ctx, func() error {
		cancel() // the clone hung to its deadline, or the client went away
		return errors.New("clone timed out")
	})
	if err == nil {
		t.Fatal("fn's error was lost")
	}
	got := states()
	if len(got) != 2 || got[0] != "Paused" || got[1] != "Resumed" {
		t.Errorf("vm states = %v, want [Paused Resumed]", got)
	}
}
