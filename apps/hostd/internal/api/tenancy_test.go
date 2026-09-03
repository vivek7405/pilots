package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// twoTenants seeds two orgs, each with one machine and one key, plus a third
// machine with no tenancy row at all -- the shape of a row created before
// tenancy existed.
func twoTenants(t *testing.T) (http.Handler, state.Store, *fakeManager) {
	t.Helper()
	h, st, fake := newTestServerWithManager(t)
	ctx := context.Background()

	// The helper already seeded m_1 (the fake's machine). Give it to org_1.
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_1", OrgID: "org_1", Kind: "machine"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	for _, m := range []struct{ id, org string }{{"m_2", "org_2"}, {"m_legacy", ""}} {
		if err := st.PutMachine(ctx, &state.Machine{
			ID: m.id, Name: m.id, HostID: "host-test", State: "running",
			Domain: m.id + ".pilotrun.app", VCPUs: 1, MemMiB: 512,
		}); err != nil {
			t.Fatalf("PutMachine: %v", err)
		}
		if m.org == "" {
			continue // no tenancy row, on purpose
		}
		if err := st.PutTenancy(ctx, &state.Tenancy{ID: m.id, OrgID: m.org, Kind: "machine"}); err != nil {
			t.Fatalf("PutTenancy: %v", err)
		}
	}
	seedKey(t, st, "pilot_org1", "org_1", "machines")
	seedKey(t, st, "pilot_org2", "org_2", "machines")
	return h, st, fake
}

func listIDs(t *testing.T, h http.Handler, path, key string) []string {
	t.Helper()
	rec := do(t, h, "GET", path, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	var rows []Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, m := range rows {
		out = append(out, m.ID)
	}
	return out
}

func TestAListShowsOnlyTheCallersOrg(t *testing.T) {
	h, _, _ := twoTenants(t)

	if got := listIDs(t, h, "/v1/machines", "pilot_org1"); len(got) != 1 || got[0] != "m_1" {
		t.Errorf("org_1 sees %v, want [m_1]", got)
	}
	if got := listIDs(t, h, "/v1/machines", "pilot_org2"); len(got) != 1 || got[0] != "m_2" {
		t.Errorf("org_2 sees %v, want [m_2]", got)
	}
	// An admin key is the ops org: it sees every row, including the one no
	// tenancy names.
	if got := listIDs(t, h, "/v1/machines", testKey); len(got) != 3 {
		t.Errorf("admin sees %v, want all three", got)
	}
	// Admin may narrow with ?org=.
	if got := listIDs(t, h, "/v1/machines?org=org_2", testKey); len(got) != 1 || got[0] != "m_2" {
		t.Errorf("admin ?org=org_2 sees %v, want [m_2]", got)
	}
	// A non-admin's ?org= is ignored, never a 403: the caller has exactly one
	// org, so the parameter can only be redundant or wrong.
	if got := listIDs(t, h, "/v1/machines?org=org_2", "pilot_org1"); len(got) != 1 || got[0] != "m_1" {
		t.Errorf("org_1 asking for org_2 sees %v, want its own [m_1]", got)
	}
}

// A foreign id is a 404 and never a 403. A 403 confirms the id exists, which
// is a machine-name oracle across tenants.
func TestAForeignIDIsNotFound(t *testing.T) {
	h, _, fake := twoTenants(t)

	rec := do(t, h, "GET", "/v1/machines/m_1", "pilot_org2")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET a foreign machine: got %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "{") {
		t.Errorf("the 404 has no JSON body: %s", rec.Body.String())
	}

	if rec := do(t, h, "DELETE", "/v1/machines/m_1", "pilot_org2"); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE a foreign machine: got %d, want 404", rec.Code)
	}
	if fake.destroyed != 0 {
		t.Errorf("a foreign delete reached the manager (%d destroys)", fake.destroyed)
	}

	// The checkpoint carries no org of its own, so a foreign checkpoint id is
	// a foreign machine: the fake's checkpoint belongs to m_1, which is
	// org_1's.
	if rec := do(t, h, "POST", "/v1/checkpoints/ck_1/restore", "pilot_org2"); rec.Code != http.StatusNotFound {
		t.Errorf("restore a foreign checkpoint: got %d, want 404", rec.Code)
	}
	if fake.restored != 0 {
		t.Errorf("a foreign restore reached the manager (%d restores)", fake.restored)
	}
}

// A row created before tenancy existed has no owner, so it is admin-only:
// showing it to any tenant would be handing one tenant another's machine.
func TestAnUntenantedRowIsAdminOnly(t *testing.T) {
	h, _, _ := twoTenants(t)

	if rec := do(t, h, "GET", "/v1/machines/m_legacy", "pilot_org1"); rec.Code != http.StatusNotFound {
		t.Errorf("a tenant sees the legacy row: got %d, want 404", rec.Code)
	}
	rec := do(t, h, "GET", "/v1/machines/m_legacy", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin cannot see the legacy row: %d", rec.Code)
	}
	var m Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.OrgID != "" {
		t.Errorf("the legacy row reports an owner %q; it has none", m.OrgID)
	}
}

// The org comes from the key. A body that names one must not be able to place
// a machine inside another tenant -- which is why the field is `json:"-"`.
func TestTheBodyCannotChooseTheOrg(t *testing.T) {
	h, _, fake := twoTenants(t)

	req := httptest.NewRequest("POST", "/v1/machines",
		strings.NewReader(`{"vcpus":1,"mem_mib":512,"org_id":"org_2"}`))
	req.Header.Set("Authorization", "Bearer pilot_org1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastCreate.OrgID != "org_1" {
		t.Errorf("the machine was created in %q; the key says org_1",
			fake.lastCreate.OrgID)
	}
}

// A tenant sees its own org on the rows it can see.
func TestAMachineReportsItsOrg(t *testing.T) {
	h, _, _ := twoTenants(t)

	rec := do(t, h, "GET", "/v1/machines/m_1", "pilot_org1")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var m Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.OrgID != "org_1" {
		t.Errorf("org_id = %q, want org_1", m.OrgID)
	}
}

// A create may name a volume to attach, and a volume is another tenant's
// data. This is the one place tenancy could be crossed by a create rather
// than by a read.
func TestACreateCannotAttachAForeignVolume(t *testing.T) {
	h, st, fake := twoTenants(t)
	ctx := context.Background()

	if err := st.PutVolume(ctx, &state.Volume{
		ID: "vol_1", Name: "data", SizeMiB: 1024, HostID: "host-test", MountPath: "/data",
	}); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "vol_1", OrgID: "org_1", Kind: "volume"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}

	rec := postJSON(t, h, "/v1/machines", "pilot_org2", `{"vcpus":1,"mem_mib":512,"volume":"vol_1"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attaching a foreign volume: got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if fake.created != 0 {
		t.Errorf("the create reached the manager anyway (%d creates)", fake.created)
	}

	// Its own tenant may attach it.
	if ok := postJSON(t, h, "/v1/machines", "pilot_org1", `{"vcpus":1,"mem_mib":512,"volume":"vol_1"}`); ok.Code != http.StatusCreated {
		t.Errorf("the owning tenant was refused its own volume: %d (%s)", ok.Code, ok.Body.String())
	}
}

// A build id becomes a machine's root filesystem, so naming a foreign one in
// a create body is the same crossing as a foreign volume, by a different
// door: it boots a shell inside another tenant's private image. Build ids are
// handed out in the NDJSON stream and in X-Pilot-Build-Id, so one has only to
// be told a single id.
func TestACreateCannotBootAForeignImage(t *testing.T) {
	h, st, fake := twoTenants(t)
	ctx := context.Background()

	if err := st.PutTenancy(ctx, &state.Tenancy{
		ID: "bld_1", OrgID: "org_1", Kind: "build",
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}

	rec := postJSON(t, h, "/v1/machines", "pilot_org2", `{"vcpus":1,"mem_mib":512,"image":"bld_1"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("booting a foreign image: got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if fake.created != 0 {
		t.Errorf("the create reached the manager anyway (%d creates)", fake.created)
	}

	// Its own tenant may boot it.
	if ok := postJSON(t, h, "/v1/machines", "pilot_org1", `{"vcpus":1,"mem_mib":512,"image":"bld_1"}`); ok.Code != http.StatusCreated {
		t.Errorf("the owning tenant was refused its own image: %d (%s)", ok.Code, ok.Body.String())
	}

	// A build nobody owns is admin-only, exactly as an unowned machine is.
	if anon := postJSON(t, h, "/v1/machines", "pilot_org2", `{"vcpus":1,"mem_mib":512,"image":"bld_unowned"}`); anon.Code != http.StatusNotFound {
		t.Errorf("an unowned build was bootable by a tenant: %d", anon.Code)
	}
}

// The build log is the build's own output -- Dockerfile lines, registry URLs,
// whatever the build echoed -- so it is scoped like the build it belongs to.
// The key here carries `deploy`, so the scope gate lets it through and the
// tenancy check is what refuses it.
func TestAForeignBuildLogIsNotReadable(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake,
		Builds: &fakeBuilder{hasLog: true, log: []BuildLogLine{{Line: "secret registry url"}}}})
	seedKey(t, st, "pilot_org2_deploy", "org_2", "deploy")
	seedKey(t, st, "pilot_org1_deploy", "org_1", "deploy")

	if err := st.PutTenancy(context.Background(), &state.Tenancy{
		ID: "bld_1", OrgID: "org_1", Kind: "build",
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/builds/bld_1/logs", nil)
	req.Header.Set("Authorization", "Bearer pilot_org2_deploy")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reading a foreign build log: got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret registry url") {
		t.Error("the refusal carried the build's own output")
	}

	// Its owner reads it.
	own := httptest.NewRequest("GET", "/v1/builds/bld_1/logs", nil)
	own.Header.Set("Authorization", "Bearer pilot_org1_deploy")
	ownRec := httptest.NewRecorder()
	h.ServeHTTP(ownRec, own)
	if ownRec.Code != http.StatusOK {
		t.Errorf("the owning tenant was refused its own build log: %d (%s)", ownRec.Code, ownRec.Body.String())
	}
}
