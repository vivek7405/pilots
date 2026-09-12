package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func forkServer(t *testing.T) (http.Handler, *fakeManager, state.Store) {
	t.Helper()
	_, st := newTestServer(t)
	fake := newFakeManager()
	ctx := t.Context()

	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_1", Name: "webapp", HostID: "host-test", State: "running",
		VCPUs: 1, MemMiB: 512, Domain: "webapp.pilotrun.app",
		UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_1", Kind: "machine", OrgID: "org_1"}); err != nil {
		t.Fatal(err)
	}
	return Routes(Deps{HostID: "host-test", Store: st, Machines: fake}), fake, st
}

// The whole point: N machines from one source, each its own machine with its
// own id. A caller that wanted a copy of a disk could already make one; what
// they could not do is start from a machine's live state.
func TestForkingMakesOneMachinePerRequestedFork(t *testing.T) {
	h, fake, _ := forkServer(t)

	rec := postJSON(t, h, "/v1/machines/m_1/fork", testKey, `{"count":3}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got ForkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forks) != 3 {
		t.Fatalf("%d forks, want 3", len(got.Forks))
	}
	ids := map[string]bool{}
	for i, f := range got.Forks {
		if f.Machine == nil {
			t.Fatalf("fork %d has no machine: %s", i, f.Error)
		}
		if ids[f.Machine.ID] {
			t.Errorf("two forks share the id %s", f.Machine.ID)
		}
		ids[f.Machine.ID] = true
	}
	if len(fake.forked) != 1 || fake.forked[0].Machine != "m_1" {
		t.Errorf("the manager was asked for %+v, want a fork of m_1", fake.forked)
	}
}

// A fork that failed does not take its siblings with it. Forks are
// independent: nine that came up are worth having when the tenth did not, and
// the caller needs to know which is which.
func TestOneFailedForkDoesNotFailTheOthers(t *testing.T) {
	h, fake, _ := forkServer(t)
	fake.forkFails = 1

	rec := postJSON(t, h, "/v1/machines/m_1/fork", testKey, `{"count":3}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got ForkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	made, failed := 0, 0
	for _, f := range got.Forks {
		if f.Machine != nil {
			made++
		}
		if f.Error != "" {
			failed++
		}
	}
	if made != 2 || failed != 1 {
		t.Errorf("%d made and %d failed, want 2 and 1", made, failed)
	}
}

// A cap, refused at the edge with a message about the request rather than
// somewhere inside a lifecycle path. One request must not be able to ask a
// host for more machines than it could ever admit.
func TestForkCountIsCapped(t *testing.T) {
	h, fake, _ := forkServer(t)

	rec := postJSON(t, h, "/v1/machines/m_1/fork", testKey, `{"count":101}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(fake.forked) != 0 {
		t.Errorf("an over-large fork reached the manager: %+v", fake.forked)
	}
}

// Forking somebody else's machine is a 404, and the manager is never asked.
// Whether a machine exists is not something a foreign caller learns by trying
// to fork it.
func TestForkingAForeignMachineIsNotFound(t *testing.T) {
	h, fake, st := forkServer(t)
	tenant := "pk_tenant_fork"
	tenantKey(t, st, tenant, "org_1", "machines")

	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_theirs", Name: "theirs", HostID: "host-test", State: "running",
		UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(t.Context(), &state.Tenancy{
		ID: "m_theirs", Kind: "machine", OrgID: "org_other",
	}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, h, "/v1/machines/m_theirs/fork", tenant, `{}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(fake.forked) != 0 {
		t.Errorf("a foreign machine was forked: %+v", fake.forked)
	}
}

// Forking a CHECKPOINT checks who owns the machine it came from.
//
// This is the door the dead `checkpoint` field on CreateMachineRequest never
// had: a checkpoint id is not itself an owned object, so without this a caller
// could name somebody else's checkpoint and be handed a machine with their
// data already in memory.
func TestForkingAForeignCheckpointIsNotFound(t *testing.T) {
	h, fake, st := forkServer(t)
	// The fake answers every checkpoint lookup with one belonging to m_1,
	// which org_1 owns. So the caller is a tenant of a DIFFERENT org: a
	// tenancy row is write-once, so the machine's owner cannot be moved, and
	// moving the caller is the honest way to express "somebody else's".
	tenant := "pk_tenant_ck"
	tenantKey(t, st, tenant, "org_outsider", "machines")

	rec := postJSON(t, h, "/v1/checkpoints/ck_1/fork", tenant, `{}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(fake.forked) != 0 {
		t.Errorf("a foreign checkpoint was forked: %+v", fake.forked)
	}
}

// The quota is charged ONCE, for every fork, before any is made. Charging per
// fork would let a request the org cannot afford create most of the machines
// before being refused.
func TestForkingIsAdmittedForEveryForkAtOnce(t *testing.T) {
	h, fake, st := forkServer(t)
	if err := st.PutQuota(t.Context(), &state.Quota{
		OrgID: "org_1", MaxMachines: 2, MaxVCPUs: 100, MaxMemMiB: 100000,
		MaxVolumeGiB: 10, MaxBuilds: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, h, "/v1/machines/m_1/fork", testKey, `{"count":10}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if len(fake.forked) != 0 {
		t.Errorf("forks were made despite the quota: %+v", fake.forked)
	}
}
