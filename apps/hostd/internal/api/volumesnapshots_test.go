package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// snapshotServer is a host holding one volume the test key's org owns.
func snapshotServer(t *testing.T) (http.Handler, *fakeManager, state.Store) {
	t.Helper()
	_, st := newTestServer(t)
	fake := newFakeManager()
	ctx := t.Context()

	if err := st.PutVolume(ctx, &state.Volume{
		ID: "vol-1", Name: "data", SizeMiB: 10240, HostID: "host-test", MountPath: "/data",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "vol-1", Kind: "volume", OrgID: "org_1"}); err != nil {
		t.Fatal(err)
	}
	return Routes(Deps{HostID: "host-test", Store: st, Machines: fake}), fake, st
}

// Taking a snapshot names the volume and answers with the stamp, which is what
// a caller holds on to in order to restore it later.
func TestASnapshotIsTakenAndNamed(t *testing.T) {
	h, fake, _ := snapshotServer(t)

	rec := postJSON(t, h, "/v1/volumes/vol-1/snapshots", testKey, ``)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got SnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != "vol-1" || got.Snapshot == "" {
		t.Errorf("response = %+v, want the volume and a stamp", got)
	}
	if len(fake.snapshotted) != 1 || fake.snapshotted[0] != "vol-1" {
		t.Errorf("snapshotted = %v, want the one volume", fake.snapshotted)
	}
}

// The list is newest first and empty rather than null for a volume nobody has
// snapshotted, which is most volumes.
func TestSnapshotsAreListedAndAnEmptyOneIsAList(t *testing.T) {
	h, _, _ := snapshotServer(t)

	req := httptest.NewRequest("GET", "/v1/volumes/vol-1/snapshots", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got SnapshotListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Snapshots == nil {
		t.Error("snapshots is null; an empty list must be a list")
	}

	// After one is taken it appears.
	if rec := postJSON(t, h, "/v1/volumes/vol-1/snapshots", testKey, ``); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Snapshots) != 1 {
		t.Errorf("snapshots = %v, want the one just taken", got.Snapshots)
	}
}

// A restore names the volume and the stamp, and reaches the manager. The
// manager is where the machine-state rules live -- a running machine is
// refused there, because that is where the machine can be seen.
func TestRestoringASnapshotReachesTheManager(t *testing.T) {
	h, fake, _ := snapshotServer(t)

	rec := postJSON(t, h, "/v1/volumes/vol-1/snapshots/20260912T101500Z/restore", testKey, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.restoredSnapshots) != 1 || fake.restoredSnapshots[0] != "vol-1@20260912T101500Z" {
		t.Errorf("restored = %v, want the volume and stamp", fake.restoredSnapshots)
	}
}

// Another org's volume is a 404, not a 403: whether a volume exists is not
// something a foreign caller learns by asking.
//
// Asked with a TENANT key. The battery's default key is admin-scoped and sees
// across orgs by design, so testing tenancy with it would assert nothing.
func TestSnapshottingAForeignVolumeIsNotFound(t *testing.T) {
	h, fake, st := snapshotServer(t)
	tenant := "pk_tenant_snapshots"
	// The machines scope: every /v1/volumes route is under it, by the prefix
	// table in auth.go, so these new routes inherit the right scope for free.
	tenantKey(t, st, tenant, "org_1", "machines")
	if err := st.PutVolume(t.Context(), &state.Volume{
		ID: "vol-theirs", Name: "theirs", SizeMiB: 1024, HostID: "host-test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(t.Context(), &state.Tenancy{
		ID: "vol-theirs", Kind: "volume", OrgID: "org_other",
	}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, h, "/v1/volumes/vol-theirs/snapshots", tenant, ``)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(fake.snapshotted) != 0 {
		t.Errorf("a foreign volume was snapshotted: %v", fake.snapshotted)
	}
}
