package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// movingStore is what a handoff looks like from the TARGET host's local
// replica: the row names the source until Take commits, then it names us.
type movingStore struct {
	state.Store
	claimed atomic.Bool
	from    string
	to      string
}

func (s *movingStore) ListMachines(context.Context) ([]state.Machine, error) {
	owner := s.from
	if s.claimed.Load() {
		owner = s.to
	}
	return []state.Machine{{
		ID: "m-1", Name: "alpha", HostID: owner,
		Domain: "alpha.pilotrun.app", State: "running",
	}}, nil
}

func (s *movingStore) ListServices(context.Context) ([]state.Service, error) {
	return nil, nil
}

func (s *movingStore) TouchMachine(context.Context, string, int64) error { return nil }

// A request for a machine being handed TO this host is held until the claim
// lands, not refused.
//
// The source suspends the machine and forwards the in-flight request straight
// away, so it arrives here BEFORE Take has committed: the row still names the
// source. The internal listener answered "machine is not served by this host"
// -- a 404 at the edge for a machine nobody has lost, out of a planned
// maintenance whose whole promise is that a request arriving mid-move is HELD
// rather than failed. Removing the drain-marker branch reds this with a 404.
func TestAForwardedRequestIsHeldUntilTheClaimLands(t *testing.T) {
	store := &movingStore{from: "host-a", to: "host-b"}

	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-b",
		Store:   store,
		Peers:   &stubPeers{addrs: map[string]string{}},
		SlotFor: func(string) (*netns.Slot, bool) { return nil, false },
	})

	// The claim commits shortly after the request arrives, the way Take does.
	go func() {
		time.Sleep(60 * time.Millisecond)
		store.claimed.Store(true)
	}()

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	req.Header.Set(forwardedHeader, "host-a")
	req.Header.Set(drainHopHeader, "host-a")
	w := httptest.NewRecorder()
	r.InternalHandler().ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatalf("the target refused a machine it was in the middle of claiming: %s",
			w.Body.String())
	}
	if !store.claimed.Load() {
		t.Error("the request was answered before the claim landed")
	}
}

// Without the marker nothing waits: an ordinary stale forward is still
// refused at once, so the hold cannot be reached by a request that is simply
// pointed at the wrong host.
func TestAnUnmarkedForwardIsStillRefusedImmediately(t *testing.T) {
	store := &movingStore{from: "host-a", to: "host-b"}

	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-b",
		Store:   store,
		Peers:   &stubPeers{addrs: map[string]string{}},
		SlotFor: func(string) (*netns.Slot, bool) { return nil, false },
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	req.Header.Set(forwardedHeader, "host-a")
	w := httptest.NewRecorder()
	start := time.Now()
	r.InternalHandler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404 for a plain stale forward", w.Code)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("an unmarked forward waited; the hold is only for a handoff")
	}
}

// A machine this host is handing OFF is never woken here, even when the
// request arrived forwarded.
//
// serveOrForward has always checked the handoff; the internal listener did
// not, and every host answers for every machine, so most requests reach some
// other host first and arrive here forwarded. That path went straight to
// serveLocally and woke a machine another host was in the middle of claiming
// -- the drain hold bypassed by one hop. Removing the HandingOff branch from
// InternalHandler makes this serve locally instead of following the machine.
func TestTheInternalListenerDoesNotWakeAMachineMidHandoff(t *testing.T) {
	var reachedTarget atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedTarget.Store(true)
		_, _ = w.Write([]byte("served by the target"))
	}))
	defer target.Close()

	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			// The row still says host-a: the handoff has not committed.
			{ID: "m-1", Name: "alpha", HostID: "host-a",
				Domain: "alpha.pilotrun.app", State: "running"},
		}},
		Peers: &stubPeers{addrs: map[string]string{
			"host-b": strings.TrimPrefix(target.URL, "http://"),
		}},
		HandingOff: func(id string) (string, bool) {
			if id == "m-1" {
				return "host-b", true
			}
			return "", false
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	req.Header.Set(forwardedHeader, "host-c")
	w := httptest.NewRecorder()
	r.InternalHandler().ServeHTTP(w, req)

	if !reachedTarget.Load() {
		t.Errorf("the request did not follow the machine; status %d, body %q",
			w.Code, w.Body.String())
	}
}

// The extra handoff hop is exactly one. A request that already carries the
// marker and still finds a machine moving away is refused, so two hosts
// disagreeing about a move cannot pass it back and forth.
func TestTheHandoffHopIsBounded(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-1", Name: "alpha", HostID: "host-a",
				Domain: "alpha.pilotrun.app", State: "running"},
		}},
		Peers:      &stubPeers{addrs: map[string]string{"host-b": "127.0.0.1:1"}},
		HandingOff: func(string) (string, bool) { return "host-b", true },
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	req.Header.Set(forwardedHeader, "host-c")
	req.Header.Set(drainHopHeader, "host-c")
	w := httptest.NewRecorder()
	r.InternalHandler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503: a second handoff hop is refused rather than "+
			"passed on", w.Code)
	}
}
