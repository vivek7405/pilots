package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// stubPeers is a fixed view of the other hosts.
type stubPeers struct {
	addrs map[string]string
	dead  map[string]bool
}

func (p *stubPeers) InternalAddr(hostID string) (string, bool) {
	a, ok := p.addrs[hostID]
	return a, ok
}

func (p *stubPeers) IsLive(hostID string) bool { return !p.dead[hostID] }

// A request for another host's machine is forwarded over the mesh, not
// refused. The client's URL is permanent, so any host must be able to answer
// for any machine.
func TestARequestForAnotherHostsMachineIsForwarded(t *testing.T) {
	var (
		gotHost      atomic.Value
		gotForwarded atomic.Value
	)
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		gotForwarded.Store(r.Header.Get(forwardedHeader))
		fmt.Fprint(w, "served by the owner")
	}))
	defer owner.Close()

	addr := strings.TrimPrefix(owner.URL, "http://")
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-1", Name: "alpha", HostID: "host-b", Domain: "alpha.pilotrun.app"},
		}},
		Peers: &stubPeers{addrs: map[string]string{"host-b": addr}},
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", w.Code, w.Body.String())
	}
	if w.Body.String() != "served by the owner" {
		t.Errorf("body = %q", w.Body.String())
	}
	// The application behind the machine builds URLs and sets cookies from the
	// Host header, so it must see what the user typed, not a mesh address.
	if got, _ := gotHost.Load().(string); got != "alpha.pilotrun.app" {
		t.Errorf("the owner saw Host %q, want the user's hostname", got)
	}
	if got, _ := gotForwarded.Load().(string); got != "host-a" {
		t.Errorf("forwarding marker = %q, want host-a", got)
	}
}

// One hop, enforced. Two hosts with briefly disagreeing views -- which happens
// during a claim -- would otherwise forward to each other until something
// timed out.
func TestTheInternalListenerRefusesASecondHop(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-b",
		Store: &stubStore{machines: []state.Machine{
			{ID: "m-1", Name: "alpha", HostID: "host-c", Domain: "alpha.pilotrun.app"},
		}},
		Peers: &stubPeers{addrs: map[string]string{"host-c": "fdcc::3"}},
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/x", nil)
	req.Host = "alpha.pilotrun.app"
	req.Header.Set(forwardedHeader, "host-a")

	w := httptest.NewRecorder()
	r.InternalHandler().ServeHTTP(w, req)

	// host-b does not own it either. It must refuse rather than forward again.
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404; a second hop would loop", w.Code)
	}
}

// Nothing should reach the internal listener without the marker: it is bound
// to the mesh and only peers speak to it.
func TestTheInternalListenerRequiresTheMarker(t *testing.T) {
	r := New(Options{Domain: "pilotrun.app", HostID: "host-a", Store: &stubStore{}})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/", nil)
	req.Host = "alpha.pilotrun.app"

	w := httptest.NewRecorder()
	r.InternalHandler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for an unmarked request", w.Code)
	}
}

// The gate: kill the host a client is mid-request against, and the next
// request succeeds. A host that finds the owner gone rescues the machine
// itself, holding the client, rather than failing until a background loop
// notices.
func TestARequestForADeadHostsMachineRescuesItHere(t *testing.T) {
	rescued := make(chan string, 1)

	store := &stubStore{machines: []state.Machine{
		{ID: "m-1", Name: "alpha", HostID: "host-dead", Domain: "alpha.pilotrun.app"},
	}}
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: store,
		Peers: &stubPeers{
			addrs: map[string]string{"host-dead": "fdcc::9"},
			dead:  map[string]bool{"host-dead": true},
		},
		Rescue: func(_ context.Context, m state.Machine) error {
			rescued <- m.ID
			// A rescue moves the row here and leaves the machine running.
			store.machines[0].HostID = "host-a"
			store.machines[0].State = "running"
			return nil
		},
		SlotFor: func(string) (*netns.Slot, bool) { return nil, false },
	})

	req := httptest.NewRequest(http.MethodGet, "http://alpha.pilotrun.app/", nil)
	req.Host = "alpha.pilotrun.app"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	select {
	case id := <-rescued:
		if id != "m-1" {
			t.Errorf("rescued %q", id)
		}
	default:
		t.Fatal("a request for a dead host's machine did not trigger a rescue")
	}
}
