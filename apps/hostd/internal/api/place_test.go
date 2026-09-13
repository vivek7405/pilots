package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakePeers resolves host ids to the test servers standing in for them.
type fakePeers map[string]string

func (p fakePeers) InternalAddr(hostID string) (string, bool) {
	addr, ok := p[hostID]
	return addr, ok
}

// countingPlacement records the outcomes, so a test can assert WHERE a create
// went rather than only that it was answered.
type countingPlacement struct {
	mu  sync.Mutex
	got []string
}

func (c *countingPlacement) Observe(outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, outcome)
}

func (c *countingPlacement) outcomes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.got...)
}

// candidate is a stand-in host that answers a create with a fixed status.
func candidate(t *testing.T, status int, hostID string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A candidate must always be told the request has been forwarded, or
		// it would rank again and the create could bounce for ever.
		if r.Header.Get(forwardedHeader) == "" {
			t.Errorf("%s received a create with no forwarded marker", hostID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(Machine{ID: "m_1", Name: "web", HostID: hostID})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// placementServer is a fleet: this host plus the candidates given, with a
// capacity row for each.
func placementServer(t *testing.T, peers fakePeers, caps ...state.HostCapacity) (http.Handler, *countingPlacement, state.Store) {
	t.Helper()
	_, st := newTestServer(t)
	ctx := t.Context()

	for _, c := range caps {
		c.UpdatedAt = time.Now().Unix()
		if err := st.PutHostCapacity(ctx, &c); err != nil {
			t.Fatal(err)
		}
		if err := st.PutHost(ctx, &state.Host{
			ID: c.HostID, LastSeen: time.Now().Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	place := &countingPlacement{}
	return Routes(Deps{
		HostID: "host-test", Store: st, Machines: newFakeManager(),
		Peers: peers, Placement: place,
	}), place, st
}

// A create that arrives at a host with less room than a peer is offered to the
// peer. Before this, a machine ran wherever the client happened to point its
// CLI, and a fleet of five hosts behaved like one host with four spares.
func TestACreateIsForwardedToTheRoomiestHost(t *testing.T) {
	roomy, calls := candidate(t, http.StatusCreated, "host-roomy")
	peers := fakePeers{"host-roomy": roomy.Listener.Addr().String()}

	h, place, _ := placementServer(t, peers,
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 1024, CPUCount: 8},
		state.HostCapacity{HostID: "host-roomy", MemFreeMiB: 32768, CPUCount: 8},
	)

	rec := postJSON(t, h, "/v1/machines", testKey, `{"name":"web","mem_mib":512}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if *calls != 1 {
		t.Errorf("the roomy host was offered the create %d times, want once", *calls)
	}
	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HostID != "host-roomy" {
		t.Errorf("the machine landed on %s, want host-roomy", got.HostID)
	}
	if outcomes := place.outcomes(); len(outcomes) != 1 || outcomes[0] != "forwarded" {
		t.Errorf("outcomes = %v, want one forward", outcomes)
	}
}

// A host that ranked well and then refuses is exactly what "the target is the
// authority" means. The create moves to the next candidate rather than
// failing, because the ranking was advisory and the refusal is information the
// ranker did not have.
func TestAFullCandidateFallsThroughToTheNext(t *testing.T) {
	full, fullCalls := candidate(t, http.StatusInsufficientStorage, "host-full")
	spare, spareCalls := candidate(t, http.StatusCreated, "host-spare")

	h, place, _ := placementServer(t, fakePeers{
		"host-full":  full.Listener.Addr().String(),
		"host-spare": spare.Listener.Addr().String(),
	},
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 512, CPUCount: 8},
		// Ranked first: it claims the most room.
		state.HostCapacity{HostID: "host-full", MemFreeMiB: 65536, CPUCount: 8},
		state.HostCapacity{HostID: "host-spare", MemFreeMiB: 32768, CPUCount: 8},
	)

	rec := postJSON(t, h, "/v1/machines", testKey, `{"name":"web","mem_mib":512}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if *fullCalls != 1 {
		t.Errorf("the full host was offered the create %d times, want once", *fullCalls)
	}
	if *spareCalls != 1 {
		t.Errorf("the spare host was offered the create %d times, want once", *spareCalls)
	}
	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HostID != "host-spare" {
		t.Errorf("the machine landed on %s, want host-spare", got.HostID)
	}
	// One refusal, then one success: the refusal is not counted as a placement
	// outcome of its own, because the create was placed exactly once.
	if outcomes := place.outcomes(); len(outcomes) != 1 || outcomes[0] != "forwarded" {
		t.Errorf("outcomes = %v, want one forward", outcomes)
	}
}

// A create that has already been forwarded once is served where it lands. Two
// hosts with briefly disagreeing views of the fleet would otherwise pass one
// create back and forth until the client gave up.
func TestAForwardedCreateIsNeverRankedAgain(t *testing.T) {
	roomy, calls := candidate(t, http.StatusCreated, "host-roomy")

	h, _, _ := placementServer(t, fakePeers{"host-roomy": roomy.Listener.Addr().String()},
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 1024, CPUCount: 8},
		state.HostCapacity{HostID: "host-roomy", MemFreeMiB: 65536, CPUCount: 8},
	)

	req := httptest.NewRequest("POST", "/v1/machines", strings.NewReader(`{"name":"web","mem_mib":512}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(forwardedHeader, "host-elsewhere")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if *calls != 0 {
		t.Errorf("a forwarded create was forwarded again, %d times", *calls)
	}
}

// A lone host has nobody to forward to and must not pay for a ranking. This is
// every single-box install, which is where most people meet the system.
func TestALoneHostServesItsOwnCreates(t *testing.T) {
	h, place, _ := placementServer(t, fakePeers{},
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 1024, CPUCount: 8},
	)

	rec := postJSON(t, h, "/v1/machines", testKey, `{"name":"web","mem_mib":512}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if outcomes := place.outcomes(); len(outcomes) != 1 || outcomes[0] != "local" {
		t.Errorf("outcomes = %v, want one local", outcomes)
	}
}

// A volume-backed create stays where it landed. The volume is claimed by
// whichever host mounts it, so forwarding the create would move the claim
// without moving the data.
func TestAVolumeCreateIsNotForwarded(t *testing.T) {
	roomy, calls := candidate(t, http.StatusCreated, "host-roomy")

	h, _, st := placementServer(t, fakePeers{"host-roomy": roomy.Listener.Addr().String()},
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 512, CPUCount: 8},
		state.HostCapacity{HostID: "host-roomy", MemFreeMiB: 65536, CPUCount: 8},
	)
	if err := st.PutVolume(t.Context(), &state.Volume{
		ID: "vol-1", Name: "data", SizeMiB: 1024, MountPath: "/data",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(t.Context(), &state.Tenancy{
		ID: "vol-1", Kind: "volume", OrgID: "org_1",
	}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, h, "/v1/machines", testKey, `{"name":"web","mem_mib":512,"volume":"vol-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if *calls != 0 {
		t.Errorf("a volume-backed create was forwarded %d times; the volume cannot follow it", *calls)
	}
}
