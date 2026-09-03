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

func putJSON(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestQuotaRoundTripsThroughTheAPI(t *testing.T) {
	h, _ := newTestServer(t)

	// With no row, the defaults are reported rather than a 404: an org with
	// no row is held to the defaults, not to nothing.
	rec := do(t, h, "GET", "/v1/quotas/org_new", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with no row: %d", rec.Code)
	}
	var got QuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MaxMachines == 0 || got.UpdatedAt != 0 {
		t.Errorf("defaults = %+v; want non-zero limits and updated_at 0", got)
	}

	put := putJSON(t, h, "/v1/quotas/org_new", testKey,
		`{"max_machines":3,"max_vcpus":6,"max_mem_mib":3072,"max_volume_gib":20,"max_builds":1}`)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT: got %d, want 200 (%s)", put.Code, put.Body.String())
	}

	rec = do(t, h, "GET", "/v1/quotas/org_new", testKey)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MaxMachines != 3 || got.MaxVCPUs != 6 || got.MaxMemMiB != 3072 ||
		got.MaxVolumeGiB != 20 || got.MaxBuilds != 1 || got.UpdatedAt == 0 {
		t.Errorf("read back %+v", got)
	}
}

// Zero is legal and freezes the org. Negative is not: it would read as an
// unreachable limit and silently admit everything.
func TestANegativeQuotaIsRefused(t *testing.T) {
	h, _ := newTestServer(t)
	if rec := putJSON(t, h, "/v1/quotas/org_new", testKey, `{"max_machines":-1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rec.Code)
	}
	if rec := putJSON(t, h, "/v1/quotas/org_new", testKey, `{"max_machines":0}`); rec.Code != http.StatusOK {
		t.Errorf("a limit of zero was refused: %d", rec.Code)
	}
}

// A refused create must not reach the manager, and the refusal must name the
// limit so a client knows what to raise.
func TestACreatePastTheQuotaIs429(t *testing.T) {
	h, st, fake := newTestServerWithManager(t)
	ctx := context.Background()
	seedKey(t, st, "pilot_org1", "org_1", "machines")
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 0, MaxVCPUs: 10, MaxMemMiB: 4096, MaxVolumeGiB: 10,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}

	rec := postJSON(t, h, "/v1/machines", "pilot_org1", `{"vcpus":1,"mem_mib":512}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	var body QuotaExceededResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "quota exceeded" || body.Quota != "machines" || body.Limit != 0 {
		t.Errorf("refusal body = %+v", body)
	}
	if fake.created != 0 {
		t.Errorf("a refused create reached the manager (%d creates)", fake.created)
	}
}

func TestAVolumePastTheQuotaIs429(t *testing.T) {
	h, st, fake := newTestServerWithManager(t)
	ctx := context.Background()
	seedKey(t, st, "pilot_org1", "org_1", "machines")
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 10, MaxVCPUs: 10, MaxMemMiB: 4096, MaxVolumeGiB: 5,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}

	rec := postJSON(t, h, "/v1/volumes", "pilot_org1", `{"name":"data","size_gib":6}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	var body QuotaExceededResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Quota != "volume_gib" || body.Limit != 5 {
		t.Errorf("refusal body = %+v, want volume_gib limit 5", body)
	}
	if fake.volumesCreated != 0 {
		t.Errorf("a refused volume create reached the manager")
	}
}

// A create that IS within the quota must carry the caller's org down to the
// manager, since that is where the tenancy row is written.
func TestAnAdmittedCreateCarriesTheOrg(t *testing.T) {
	h, st, fake := newTestServerWithManager(t)
	seedKey(t, st, "pilot_org1", "org_1", "machines")

	if rec := postJSON(t, h, "/v1/machines", "pilot_org1", `{"vcpus":1,"mem_mib":512}`); rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastCreate.OrgID != "org_1" {
		t.Errorf("the manager was passed org %q, want org_1", fake.lastCreate.OrgID)
	}

	if rec := postJSON(t, h, "/v1/volumes", "pilot_org1", `{"name":"data","size_gib":1}`); rec.Code != http.StatusCreated {
		t.Fatalf("volume: got %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastCreateVolume.OrgID != "org_1" {
		t.Errorf("the volume was created in %q, want org_1", fake.lastCreateVolume.OrgID)
	}
}
