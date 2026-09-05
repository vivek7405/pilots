package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeSealer is a fleet key that seals visibly, so a test can tell a sealed
// value from a plaintext one without holding a real key.
type fakeSealer struct{ set bool }

func (f fakeSealer) IsSet() bool { return f.set }

func (f fakeSealer) Seal(raw []byte) (string, error) {
	return "sealed:" + string(raw), nil
}

// serviceServer seeds one service owned by org_1 and returns a server holding
// a fleet key, plus the store so a test can read the row back.
func serviceServer(t *testing.T, key Sealer) (http.Handler, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")

	ctx := context.Background()
	if err := st.PutService(ctx, &state.Service{
		ID: "s_1", Name: "web", Replicas: 1, Domain: "web", CreatedAt: 1,
		Repo: "vivek7405/shop", Branch: "main", Env: `{"A":"1"}`,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{
		ID: "s_1", OrgID: "org_1", Kind: "service", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	return Routes(Deps{
		HostID: "host-test", Store: st, Domain: "pilotrun.app", FleetKey: key,
	}), st
}

func getService(t *testing.T, st state.Store) *state.Service {
	t.Helper()
	svc, err := st.GetService(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	return svc
}

func TestPatchUpdatesReplicasAndSealsSecrets(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{
		"replicas":   3,
		"secret_env": map[string]string{"K": "v"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got Service
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Replicas != 3 {
		t.Errorf("response replicas = %d, want 3", got.Replicas)
	}

	row := getService(t, st)
	if row.Replicas != 3 {
		t.Errorf("stored replicas = %d, want 3", row.Replicas)
	}
	// Sealed by hostd, never by the client: a client that sealed would need
	// the fleet key, and it would stop being fleet infrastructure.
	if !strings.HasPrefix(row.EnvSealed, "sealed:") {
		t.Errorf("env_sealed = %q, want it sealed", row.EnvSealed)
	}
	if strings.Contains(row.Env, "\"K\"") {
		t.Errorf("a secret landed in the plaintext env: %q", row.Env)
	}
	// The response never carries either half.
	if strings.Contains(rec.Body.String(), "sealed:") || strings.Contains(rec.Body.String(), `"v"`) {
		t.Errorf("the response leaked the environment: %s", rec.Body)
	}
}

// The one field the clients must not be able to set here. Knobs travel on the
// deploy, because a service row has no knobs column and replica rows are
// single-writer to their own hosts.
func TestPatchRefusesKnobsByName(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{
		"knobs": map[string]any{"auto_stop": "off"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "knobs") {
		t.Errorf("the 400 does not name the field: %s", rec.Body)
	}
	// And nothing else in the body was applied: the whole patch is refused.
	if row := getService(t, st); row.Replicas != 1 {
		t.Errorf("a refused patch still wrote: replicas = %d", row.Replicas)
	}
}

// Any unknown field, not just knobs. A key silently dropped is a compose file
// that says one thing and a service that does another.
func TestPatchRefusesAnyUnknownField(t *testing.T) {
	h, _ := serviceServer(t, fakeSealer{set: true})
	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"replicaz": 2})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "replicaz") {
		t.Errorf("the 400 does not name the typo: %s", rec.Body)
	}
}

func TestPatchRefusesSecretsWithoutAFleetKey(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: false})
	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{
		"secret_env": map[string]string{"K": "v"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "PILOT_FLEET_KEY") {
		t.Errorf("the 400 does not say what to set: %s", rec.Body)
	}
	if row := getService(t, st); row.EnvSealed != "" {
		t.Errorf("a secret was stored without a key: %q", row.EnvSealed)
	}
}

// The dashboard disconnects a repo by sending repo: "". Only a pointer field
// can carry that, which is why the whole body is pointers.
func TestPatchClearsAFieldOnAnExplicitEmptyValue(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"repo": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	row := getService(t, st)
	if row.Repo != "" {
		t.Errorf("repo = %q, want it cleared", row.Repo)
	}
	// Everything absent from the body is untouched.
	if row.Branch != "main" || row.Replicas != 1 {
		t.Errorf("an absent field moved: %+v", row)
	}
}

// An empty map clears; an absent one is untouched. Both are needed, and only a
// nil check tells them apart.
func TestPatchDistinguishesAnEmptyEnvFromAnAbsentOne(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})

	if rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"branch": "next"}); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if row := getService(t, st); row.Env != `{"A":"1"}` {
		t.Errorf("an absent env cleared the stored one: %q", row.Env)
	}

	if rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"env": map[string]string{}}); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if row := getService(t, st); row.Env != "" {
		t.Errorf("env = %q, want an explicit empty map to clear it", row.Env)
	}
}

// Env REPLACES rather than merges. The client merges when it wants a merge,
// because only the client knows which of the two it meant.
func TestPatchReplacesTheStoredEnv(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{
		"env": map[string]string{"B": "2"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if row := getService(t, st); row.Env != `{"B":"2"}` {
		t.Errorf("env = %q, want the body's map and nothing of the old one", row.Env)
	}
}

// The merged row is what is validated, not the body: scaling a routable
// service to zero is fine, scaling an unreachable one to zero is not.
func TestPatchRefusesAServiceNothingCouldEverWake(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})

	// This one has a domain, so zero replicas is legal.
	if rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"replicas": 0}); rec.Code != http.StatusOK {
		t.Fatalf("scaling a routable service to zero: got %d: %s", rec.Code, rec.Body)
	}

	ctx := context.Background()
	if err := st.PutService(ctx, &state.Service{ID: "s_2", Name: "worker", Replicas: 1, CreatedAt: 1}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "s_2", OrgID: "org_1", Kind: "service", CreatedAt: 1}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	rec := doJSON(t, h, "PATCH", "/v1/services/s_2", map[string]any{"replicas": 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "can never be reached or woken") {
		t.Errorf("the 400 does not say why: %s", rec.Body)
	}
	if rec := doJSON(t, h, "PATCH", "/v1/services/s_2", map[string]any{"replicas": -1}); rec.Code != http.StatusBadRequest {
		t.Errorf("negative replicas: got %d, want 400", rec.Code)
	}
}

// A foreign service is a 404, not a 403: telling a caller that an id exists is
// already telling them something.
func TestPatchAndReleasesHideAnotherOrgsService(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	// A tenant-scoped key, not an admin one: an admin key sees every org by
	// design, so it could not show this.
	other := seedKey(t, st, "pilot_other", "org_2", "deploy")

	for _, tc := range []struct{ method, path string }{
		{"PATCH", "/v1/services/s_1"},
		{"GET", "/v1/services/s_1/releases"},
	} {
		rec := do(t, h, tc.method, tc.path, other)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404", tc.method, tc.path, rec.Code)
		}
		// JSON, not the mux's bare text: a client has to be able to parse it.
		if !strings.Contains(rec.Body.String(), "{") {
			t.Errorf("%s %s: body is not JSON: %s", tc.method, tc.path, rec.Body)
		}
	}
}

func TestReleasesAreNewestFirstAndEmptyIsAnArray(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})

	rec := do(t, h, "GET", "/v1/services/s_1/releases", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	// [] and not null: the dashboard maps over this on every service page.
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("no releases answered %s, want []", rec.Body)
	}

	ctx := context.Background()
	for _, rel := range []state.Release{
		{ID: "rel_old", ServiceID: "s_1", RootfsBuildID: "b1", Healthy: true, CreatedAt: 100},
		{ID: "rel_new", ServiceID: "s_1", RootfsBuildID: "b2", CreatedAt: 200},
	} {
		if err := st.PutRelease(ctx, &rel); err != nil {
			t.Fatalf("PutRelease: %v", err)
		}
	}

	rec = do(t, h, "GET", "/v1/services/s_1/releases", testKey)
	var got []Release
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d releases, want 2", len(got))
	}
	if got[0].ID != "rel_new" {
		t.Errorf("first release is %s, want the newest", got[0].ID)
	}
	for _, rel := range got {
		if rel.ServiceID != "s_1" {
			t.Errorf("release %s carries service_id %q", rel.ID, rel.ServiceID)
		}
	}
	if !got[1].Healthy {
		t.Errorf("healthy was dropped on the way out: %+v", got[1])
	}
}

// A missing service is a 404 on both routes, distinguishable from a foreign
// one only by which org asked.
func TestPatchOnAMissingServiceIs404(t *testing.T) {
	h, _ := serviceServer(t, fakeSealer{set: true})
	rec := doJSON(t, h, "PATCH", "/v1/services/s_nope", map[string]any{"replicas": 2})
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404: %s", rec.Code, rec.Body)
	}
}

// The wire shape's pointers survive a round trip. Repo as a plain string would
// make the dashboard's disconnect indistinguishable from an absent field, and
// nothing else in the stack would notice.
func TestUpdateServiceRequestKeepsAnExplicitEmptyString(t *testing.T) {
	var req UpdateServiceRequest
	if err := json.Unmarshal([]byte(`{"repo":""}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Repo == nil || *req.Repo != "" {
		t.Errorf("repo = %v, want a non-nil pointer to the empty string", req.Repo)
	}
	if req.Branch != nil {
		t.Errorf("branch = %v, want nil for an absent field", req.Branch)
	}
}
