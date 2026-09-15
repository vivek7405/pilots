package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A replay works when the machine is on ANOTHER host, and its header never
// reaches the client.
//
// The forwarding proxy had no ModifyResponse, which made this the one case
// where Pilot-Replay did nothing. The owning host's serveLocally sees no
// replay state -- the state is a context value on the EDGE's request and does
// not travel over HTTP -- so it re-sets the header for the edge to act on,
// exactly as designed. The edge then passed it straight through: the
// application's routing instruction was silently ignored, and a header naming
// an internal machine reached the client.
//
// An app using fly-replay's idiom therefore got a working replay or a silent
// no-op depending on which host its machine happened to be on, which is
// invariant 2 broken in the direction hardest to notice. Removing
// forwardTo's ModifyResponse reds both assertions below.
func TestAReplayWorksAcrossHosts(t *testing.T) {
	var passes atomic.Int64
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ReplaySrcHeader) == "" {
			// First pass: the application asks for a different machine.
			passes.Add(1)
			w.Header().Set(ReplayHeader, "machine=beta;state=tenant-7")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("this response must never reach the client"))
			return
		}
		passes.Add(1)
		_, _ = w.Write([]byte("served after the replay, src=" + r.Header.Get(ReplaySrcHeader)))
	}))
	defer owner.Close()

	alpha := state.Machine{
		ID: "m-1", Name: "alpha", App: "shop", HostID: "host-b",
		Domain: "alpha.pilotrun.app", State: "running",
	}
	beta := state.Machine{
		ID: "m-2", Name: "beta", App: "shop", HostID: "host-b",
		Domain: "beta.pilotrun.app", State: "running",
	}
	addr := strings.TrimPrefix(owner.URL, "http://")
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{alpha, beta}},
		Peers: &stubPeers{addrs: map[string]string{"host-b": addr}},
		Lookup: func(name string) (state.Machine, bool) {
			if name == "beta" {
				return beta, true
			}
			return state.Machine{}, false
		},
		OrgOf: func(_ context.Context, id string) (string, bool) {
			return "org-1", id == "m-1" || id == "m-2"
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), "served after the replay") {
		t.Errorf("body = %q; the replay was a no-op and the first machine's own "+
			"response reached the client", w.Body.String())
	}
	if got := w.Header().Get(ReplayHeader); got != "" {
		t.Errorf("%s = %q reached the client; a routing instruction naming an "+
			"internal machine is never a client's to see", ReplayHeader, got)
	}
	if !strings.Contains(w.Body.String(), "tenant-7") {
		t.Errorf("the second machine was not told the state the first one carried: %q",
			w.Body.String())
	}
	if got := passes.Load(); got != 2 {
		t.Errorf("the owner saw %d requests, want two: one answer and one replay", got)
	}
}

// A replay a cross-host machine asks for that it may not have is refused, and
// the refusal is the edge's, not a header passed to the client.
func TestACrossHostReplayToAnotherOrgIsRefused(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ReplayHeader, "machine=victim")
		_, _ = w.Write([]byte("nope"))
	}))
	defer owner.Close()

	alpha := state.Machine{
		ID: "m-1", Name: "alpha", App: "shop", HostID: "host-b",
		Domain: "alpha.pilotrun.app", State: "running",
	}
	victim := state.Machine{
		ID: "m-9", Name: "victim", App: "shop", HostID: "host-b",
		Domain: "victim.pilotrun.app", State: "running",
	}
	addr := strings.TrimPrefix(owner.URL, "http://")
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{alpha, victim}},
		Peers: &stubPeers{addrs: map[string]string{"host-b": addr}},
		Lookup: func(name string) (state.Machine, bool) {
			if name == "victim" {
				return victim, true
			}
			return state.Machine{}, false
		},
		OrgOf: func(_ context.Context, id string) (string, bool) {
			if id == "m-9" {
				return "org-2", true
			}
			return "org-1", true
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502: a cross-org replay must be refused at the "+
			"edge wherever the asking machine runs", w.Code)
	}
	if w.Body.String() == "nope" {
		t.Error("the refusing machine's own body reached the client")
	}
	if got := w.Header().Get(ReplayHeader); got != "" {
		t.Errorf("%s = %q reached the client", ReplayHeader, got)
	}
}
