package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeDrainer records what it was asked to do.
type fakeDrainer struct {
	mu       sync.Mutex
	drains   int
	undrains int
	draining bool
	taken    []string
	// picked is where the drainer was told each machine could go, so a test
	// can assert the ranking reached it.
	picked []string
	err    error
}

func (f *fakeDrainer) Drain(ctx context.Context, pick func(state.Machine) (string, bool)) (*DrainReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
	if f.err != nil {
		return nil, f.err
	}
	f.draining = true
	if target, ok := pick(state.Machine{ID: "m_1", Name: "web", MemMiB: 512}); ok {
		f.picked = append(f.picked, target)
	}
	return &DrainReport{Moved: []string{"m_1"}}, nil
}

func (f *fakeDrainer) Undrain() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.undrains++
	f.draining = false
}

func (f *fakeDrainer) Draining() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draining
}

func (f *fakeDrainer) Take(_ context.Context, machineID, handoffID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken = append(f.taken, machineID+"/"+handoffID)
	return f.err
}

func drainServer(t *testing.T) (http.Handler, *fakeDrainer, state.Store) {
	t.Helper()
	_, st := newTestServer(t)
	d := &fakeDrainer{}
	return Routes(Deps{
		HostID: "host-test", Store: st, Machines: newFakeManager(), Drain: d,
	}), d, st
}

// A drain empties the host it names and leaves it refusing new work. The flag
// outliving the call is the point: a host that started taking machines again
// the moment the drain returned would refill before the operator could reboot
// it.
func TestDrainingAHostEmptiesItAndLeavesItRefusing(t *testing.T) {
	h, d, _ := drainServer(t)

	rec := postJSON(t, h, "/v1/hosts/host-test/drain", testKey, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got DrainReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Moved) != 1 || got.Moved[0] != "m_1" {
		t.Errorf("moved = %v, want the one machine", got.Moved)
	}
	if !got.Draining {
		t.Error("the host is not marked draining after a drain")
	}
	if d.drains != 1 {
		t.Errorf("drains = %d, want one", d.drains)
	}
}

// Undraining lets a host take work again and moves NOTHING back. Machines are
// where they are; moving them a second time for tidiness would be a second
// outage for no benefit.
func TestUndrainingLetsAHostTakeWorkAgain(t *testing.T) {
	h, d, _ := drainServer(t)

	if rec := postJSON(t, h, "/v1/hosts/host-test/drain", testKey, ``); rec.Code != http.StatusOK {
		t.Fatalf("drain: %d", rec.Code)
	}
	req := httptest.NewRequest("DELETE", "/v1/hosts/host-test/drain", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("undrain: %d: %s", rec.Code, rec.Body.String())
	}
	if d.undrains != 1 {
		t.Errorf("undrains = %d, want one", d.undrains)
	}
	if d.Draining() {
		t.Error("the host is still draining after an undrain")
	}
}

// A drain moves every org's machines at once, which is an operator action on
// the FLEET. A tenant-scoped key has no business asking for it.
func TestDrainingNeedsAnAdminKey(t *testing.T) {
	_, st := newTestServer(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: newFakeManager(), Drain: &fakeDrainer{}})

	// A key scoped to one org, which is what every tenant holds.
	tenant := "pk_tenant_only"
	tenantKey(t, st, tenant, "org_1", "machines")

	rec := postJSON(t, h, "/v1/hosts/host-test/drain", tenant, ``)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "admin") {
		t.Errorf("the refusal does not say which scope is missing: %s", rec.Body.String())
	}
}

// The take route is authorised by the OFFER, not by the call, so the handler's
// job is only to pass the id along. A call naming no offer is refused before
// anything is asked of the store.
func TestTakeRequiresAHandoffID(t *testing.T) {
	h, d, _ := drainServer(t)

	if rec := postJSON(t, h, "/v1/machines/m_1/take", testKey, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty take got %d, want 400", rec.Code)
	}
	if len(d.taken) != 0 {
		t.Errorf("a take with no offer reached the manager: %v", d.taken)
	}

	rec := postJSON(t, h, "/v1/machines/m_1/take", testKey, `{"handoff_id":"ho-1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if len(d.taken) != 1 || d.taken[0] != "m_1/ho-1" {
		t.Errorf("taken = %v, want the machine and its offer", d.taken)
	}
}

// The status route says what is still here, so an operator can watch a drain
// converge rather than guess at it.
func TestDrainStatusListsWhatIsStillHere(t *testing.T) {
	h, _, st := drainServer(t)
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_left", Name: "left", HostID: "host-test", State: "running",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/hosts/host-test/drain", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got DrainReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range got.Left {
		if id == "m_left" {
			found = true
		}
	}
	if !found {
		t.Errorf("left = %v, want the machine still on this host", got.Left)
	}
}
