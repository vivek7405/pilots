package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// setEdgeHeaders alone, without a proxy in the way: the three headers it owns
// and nothing else.
func TestSetEdgeHeadersOwnsTheForwardedTriple(t *testing.T) {
	for _, tc := range []struct {
		name      string
		url       string
		inbound   string
		wantProto string
	}{
		{"plain http drops a forged client", "http://alpha.pilotrun.app/x", "1.2.3.4", "http"},
		{"tls says https", "https://alpha.pilotrun.app/x", "", "https"},
		{"a forged chain goes too", "http://alpha.pilotrun.app/x", "1.2.3.4, 5.6.7.8", "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.Host = "alpha.pilotrun.app"
			if tc.inbound != "" {
				req.Header.Set("X-Forwarded-For", tc.inbound)
			}

			setEdgeHeaders(req)

			// Deleted, not appended to: ReverseProxy adds the peer to
			// whatever survives, and a forged entry would sit leftmost.
			if got := req.Header.Get("X-Forwarded-For"); got != "" {
				t.Errorf("X-Forwarded-For = %q, want it deleted", got)
			}
			if got := req.Header.Get("X-Forwarded-Proto"); got != tc.wantProto {
				t.Errorf("X-Forwarded-Proto = %q, want %q", got, tc.wantProto)
			}
			if got := req.Header.Get("X-Forwarded-Host"); got != "alpha.pilotrun.app" {
				t.Errorf("X-Forwarded-Host = %q, want the name the user typed", got)
			}
		})
	}
}

// edgeRouter builds a router serving one running local machine, with the guest
// replaced by a recording HTTP server.
func edgeRouter(t *testing.T, guest http.Handler) *Router {
	t.Helper()

	fake := httptest.NewServer(guest)
	t.Cleanup(fake.Close)

	prev := agentAddr
	agentAddr = func(*netns.Slot) string { return strings.TrimPrefix(fake.URL, "http://") }
	t.Cleanup(func() { agentAddr = prev })

	store := &stubStore{machines: []state.Machine{
		{ID: "m-1", Name: "alpha", HostID: "host-a", State: "running", Domain: "alpha.pilotrun.app"},
	}}
	return New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store:   store,
		Manager: machines.New(machines.Options{HostID: "host-a", Store: store}),
		SlotFor: func(string) (*netns.Slot, bool) { return &netns.Slot{Idx: 1}, true },
	})
}

// The guest must see the peer alone. A caller that sends its own
// X-Forwarded-For is choosing a rate-limit bucket otherwise, because every
// limiter behind us reads the leftmost entry.
func TestAForgedForwardedForDoesNotReachTheGuest(t *testing.T) {
	var got http.Header
	r := edgeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = req.Header.Clone()
	}))

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.ServeHTTP(httptest.NewRecorder(), req)

	// 192.0.2.1 is what httptest.NewRequest stamps as RemoteAddr, so it is
	// the peer ReverseProxy appends once the forged value is gone.
	if want := "192.0.2.1"; got.Get("X-Forwarded-For") != want {
		t.Errorf("the guest saw X-Forwarded-For %q, want %q alone",
			got.Get("X-Forwarded-For"), want)
	}
	if got.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", got.Get("X-Forwarded-Proto"))
	}
	if got.Get("X-Forwarded-Host") != "alpha.pilotrun.app" {
		t.Errorf("X-Forwarded-Host = %q", got.Get("X-Forwarded-Host"))
	}
}

// TLS terminates at the router, so the guest learns the scheme only from what
// the router sets. webjs turns HSTS on for https and nothing else.
func TestTLSAtTheEdgeReachesTheGuestAsHTTPS(t *testing.T) {
	var got http.Header
	var gotHost string
	r := edgeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = req.Header.Clone()
		gotHost = req.Host
	}))

	req := httptest.NewRequest(http.MethodGet, "https://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	r.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", got.Get("X-Forwarded-Proto"))
	}
	// Host stays what the user typed: applications build absolute URLs and
	// set cookies from it.
	if gotHost != "alpha.pilotrun.app" || got.Get("X-Forwarded-Host") != "alpha.pilotrun.app" {
		t.Errorf("Host = %q, X-Forwarded-Host = %q", gotHost, got.Get("X-Forwarded-Host"))
	}
}
