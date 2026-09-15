package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A request for a machine being MOVED follows the machine.
//
// This is the assertion that makes a drain invisible. During a handoff the row
// still names the source host while the machine has already been suspended
// there and offered elsewhere, so without this the request is served by the
// source, finds nothing to wake, and fails -- a customer-visible failure for a
// planned operation nobody asked them to notice.
func TestARequestForAMachineMidDrainFollowsIt(t *testing.T) {
	var servedBy atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedBy.Store("target")
		fmt.Fprint(w, "served by the target")
	}))
	defer target.Close()

	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			// The row still says host-a: the handoff has not completed.
			{ID: "m-1", Name: "alpha", HostID: "host-a", Domain: "alpha.pilotrun.app"},
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
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := servedBy.Load(); got != "target" {
		t.Errorf("the request was not served by the target host: %v", got)
	}
}

// A machine that is NOT moving is never forwarded. The handoff check runs on
// every request, so it has to be invisible when nothing is moving -- which is
// almost always.
func TestAnOrdinaryRequestIsUnaffectedByTheDrainCheck(t *testing.T) {
	var forwarded atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Store(true)
		fmt.Fprint(w, "forwarded")
	}))
	defer peer.Close()

	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-1", Name: "alpha", HostID: "host-a", Domain: "alpha.pilotrun.app",
				State: "running"},
		}},
		Peers: &stubPeers{addrs: map[string]string{
			"host-b": strings.TrimPrefix(peer.URL, "http://"),
		}},
		// Nothing is moving.
		HandingOff: func(string) (string, bool) { return "", false },
		SlotFor:    func(string) (*netns.Slot, bool) { return nil, false },
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// What it answered does not matter here -- the machine has no slot, so the
	// local path has nothing to dial. What matters is that it was not sent to
	// another host.
	if forwarded.Load() {
		t.Error("a machine nobody is moving was forwarded to another host")
	}
}
