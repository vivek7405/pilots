package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A reset aimed at ANOTHER host's builder is forwarded there, not run here.
//
// Destroy writes the machine's rows, and a host writes only rows describing
// its own machines (invariant 1). Run locally against a peer's builder it
// either fails outright or writes a row this host does not own -- which does
// not error, it corrupts state through a CRDT merge. Either way the reset
// reported success and the wedged builder was still running, which is the one
// failure `pilot builder reset` exists to end.
//
// Removing the forward makes this destroy the builder locally and never reach
// the peer.
func TestAResetForAnotherHostIsForwardedToIt(t *testing.T) {
	var reached atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.Header.Get(forwardedHeader) == "" {
			t.Error("the peer was not told the request had been forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "epoch": 9, "destroyed": 1})
	}))
	defer peer.Close()

	_, st, fake := newTestServerWithManager(t)
	fb := &fakeBuilder{}
	h := Routes(Deps{
		HostID: "host-test", Store: st, Machines: fake, Builds: fb,
		Peers: fakePeers{"host-other": peer.Listener.Addr().String()},
	})
	// A builder row for the OTHER host, which this host must not touch.
	seedBuilder(t, st, "m_builder", "host-other", "org_1")

	rec := do(t, h, "POST", "/v1/builders/host-other/reset", testKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if reached.Load() != 1 {
		t.Errorf("the peer was asked %d times, want once", reached.Load())
	}
	if fake.destroyed != 0 {
		t.Errorf("this host destroyed %d of another host's builders", fake.destroyed)
	}
	if fb.epoch != 0 {
		t.Errorf("this host bumped the epoch to %d as well as forwarding; the "+
			"forwarded request already did it", fb.epoch)
	}
}

// A host name this fleet cannot place still advances the epoch here.
//
// The two halves of a reset answer two complaints. "This builder is wedged" is
// host-scoped and belongs where the builder is; "my layers are wrong" is
// org-wide and has to be fixable without knowing which host holds a machine.
// Forwarding unconditionally would have turned the second into a 503 for
// anybody who did not already know the answer.
func TestAnUnplaceableHostStillAdvancesTheEpoch(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	fb := &fakeBuilder{}
	h := Routes(Deps{
		HostID: "host-test", Store: st, Machines: fake, Builds: fb,
		Peers: fakePeers{},
	})

	rec := do(t, h, "POST", "/v1/builders/host-nowhere/reset", testKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if fb.epoch != 1 {
		t.Errorf("epoch is %d, want 1: the org-wide half does not need a host", fb.epoch)
	}
	if fake.destroyed != 0 {
		t.Errorf("destroyed %d machines for a host that is not in the fleet", fake.destroyed)
	}
}
