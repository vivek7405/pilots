package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// seedBuilder writes a builder row and its tenancy, the way hostd's own
// EnsureBuilder does.
func seedBuilder(t *testing.T, st state.Store, id, hostID, org string) {
	t.Helper()
	row := &state.Machine{
		ID: id, Name: quota.BuilderNamePrefix + org + "-" + hostID,
		HostID: hostID, State: "running",
		VCPUs: 2, MemMiB: 2048, Domain: id + ".pilotrun.app",
		UpdatedAt: time.Now().Unix(),
	}
	if err := st.PutMachine(context.Background(), row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := st.PutTenancy(context.Background(),
		&state.Tenancy{ID: id, Kind: "machine", OrgID: org}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
}

// A builder is infrastructure hostd made for itself. An agent listing machines
// to pick one to exec into should not have to know to skip it, and a dashboard
// list that shows it invites someone to destroy the thing their next deploy
// needs.
func TestBuildersAreAbsentFromTheMachineListUnlessAsked(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	seedBuilder(t, st, "m_builder", "host-test", "org_1")

	rec := do(t, h, "GET", "/v1/machines", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var listed []Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range listed {
		if m.ID == "m_builder" {
			t.Fatalf("a builder is in the default machine list: %+v", m)
		}
	}

	rec = do(t, h, "GET", "/v1/machines?include=builders", testKey)
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, m := range listed {
		if m.ID == "m_builder" {
			found = true
		}
	}
	if !found {
		t.Error("?include=builders did not include the builder")
	}
}

func TestListBuildersAnswersOnlyBuilders(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	seedBuilder(t, st, "m_builder", "host-test", "org_1")

	rec := do(t, h, "GET", "/v1/builders", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Builders []Machine `json:"builders"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Builders) != 1 || got.Builders[0].ID != "m_builder" {
		t.Fatalf("builders = %+v, want exactly the builder row", got.Builders)
	}
	if got.Builders[0].HostID != "host-test" {
		t.Errorf("the builder does not say which host it is on: %+v", got.Builders[0])
	}
}

// Reset does two things that answer two different complaints: destroy the
// wedged builder on one host, and advance the cache generation so every OTHER
// host drops its copy at its next build. No message is sent to those hosts.
func TestResetDestroysTheBuilderAndAdvancesTheEpoch(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	fb := &fakeBuilder{}
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb})
	seedBuilder(t, st, "m_builder", "host-test", "org_1")

	rec := do(t, h, "POST", "/v1/builders/host-test/reset", testKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Epoch     int `json:"epoch"`
		Destroyed int `json:"destroyed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Destroyed != 1 || fake.destroyed != 1 {
		t.Errorf("destroyed %d builders (manager saw %d), want 1", got.Destroyed, fake.destroyed)
	}
	if got.Epoch != 1 || fb.epoch != 1 {
		t.Errorf("epoch is %d (builder saw %d), want 1", got.Epoch, fb.epoch)
	}
}

// The second complaint on its own: "my layers are wrong" must be fixable
// without knowing which host holds a machine, so the epoch moves even when
// this host has no builder to destroy.
func TestResetAdvancesTheEpochWithNoBuilderToDestroy(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	fb := &fakeBuilder{}
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb})

	rec := do(t, h, "POST", "/v1/builders/host-empty/reset", testKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if fb.epoch != 1 {
		t.Errorf("epoch is %d, want 1 even with nothing to destroy", fb.epoch)
	}
	if fake.destroyed != 0 {
		t.Errorf("destroyed %d machines on a host with no builder", fake.destroyed)
	}
}

// A reset names an org's cache. Resetting on behalf of one org must never
// advance another's, since that would cost every host in the fleet a cold
// build for a tenant who asked for nothing.
func TestResetLeavesAnotherOrgsBuilderAlone(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	fb := &fakeBuilder{}
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb})
	seedBuilder(t, st, "m_other", "host-test", "org_2")

	rec := do(t, h, "POST", "/v1/builders/host-test/reset?org=org_1", testKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if fake.destroyed != 0 {
		t.Errorf("destroyed %d machines; another org's builder must be left alone", fake.destroyed)
	}
	if fb.bumpedOrg != "org_1" {
		t.Errorf("advanced the epoch of %q, want org_1", fb.bumpedOrg)
	}
}

func TestIncludeBuildersParsesTheParameter(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?include=builders", true},
		{"?include=labels,builders", true},
		{"?include=builders&include=other", true},
		{"?include=other", false},
		{"?include=builder", false},
	} {
		r, err := http.NewRequest("GET", "/v1/machines"+tc.query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := includeBuilders(r); got != tc.want {
			t.Errorf("includeBuilders(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}
