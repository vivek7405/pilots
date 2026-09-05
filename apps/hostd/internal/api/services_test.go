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

// A scale-up is a create of that many machines, and it is the ONLY place the
// count can grow: the deploy admits one replica's headroom whatever the row
// says, and the rollout creates its replicas through the manager rather than
// through this API. Unchecked, an org frozen at one machine patches itself to
// a hundred and the next deploy boots them.
func TestAPatchPastTheQuotaIs429AndDoesNotWrite(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	if err := st.PutQuota(context.Background(), &state.Quota{
		OrgID: "org_1", MaxMachines: 1, MaxVCPUs: 10, MaxMemMiB: 8192, MaxVolumeGiB: 10,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"replicas": 5})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429: %s", rec.Code, rec.Body)
	}
	var body QuotaExceededResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Quota != "machines" || body.Limit != 1 {
		t.Errorf("refusal body = %+v, want the machines limit named", body)
	}
	if got := getService(t, st).Replicas; got != 1 {
		t.Errorf("a refused patch was written anyway: replicas = %d", got)
	}

	// Scaling DOWN is never refused: it hands capacity back.
	if rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"replicas": 0}); rec.Code != http.StatusOK {
		t.Fatalf("a scale-down was refused: %d %s", rec.Code, rec.Body)
	}
}

// seedVolume adds an unattached volume owned by org_1.
func seedVolume(t *testing.T, st state.Store, id string, machineID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.PutVolume(ctx, &state.Volume{
		ID: id, Name: id, SizeMiB: 1024, HostID: "host-test",
		MountPath: "/data", MachineID: machineID,
	}); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: id, OrgID: "org_1", Kind: "volume"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
}

// createService posts a service create and returns the recorder.
func createService(t *testing.T, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, "POST", "/v1/services", body)
}

// The binding is the whole point: without a row here the volume reaches
// nothing, the replica boots on ephemeral disk, and the first replacement is
// where anyone finds out.
func TestACreateStoresAndReturnsItsVolume(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	seedVolume(t, st, "vol_1", "")

	rec := createService(t, h, map[string]any{
		"name": "db", "app": "shop", "replicas": 1, "volume": "vol_1",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", rec.Code, rec.Body)
	}
	var out Service
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.VolumeID != "vol_1" {
		t.Errorf("the create returned volume_id %q, want vol_1", out.VolumeID)
	}

	sv, err := st.ServiceVolume(context.Background(), out.ID)
	if err != nil {
		t.Fatalf("ServiceVolume: %v", err)
	}
	if sv.VolumeID != "vol_1" || sv.Ordinal != 1 {
		t.Errorf("binding is %+v, want vol_1 at ordinal 1", *sv)
	}

	// And every read carries it: a client that cannot see the volume cannot
	// tell a volume-backed service from an ephemeral one.
	get := doJSON(t, h, "GET", "/v1/services/"+out.ID, nil)
	var one Service
	if err := json.Unmarshal(get.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if one.VolumeID != "vol_1" {
		t.Errorf("GET returned volume_id %q, want vol_1", one.VolumeID)
	}
	list := doJSON(t, h, "GET", "/v1/services", nil)
	var all []Service
	if err := json.Unmarshal(list.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var found bool
	for _, s := range all {
		if s.ID == out.ID {
			found = s.VolumeID == "vol_1"
		}
	}
	if !found {
		t.Errorf("the list did not carry volume_id for %s: %+v", out.ID, all)
	}
}

// A volume is mounted by exactly one machine, so a service that mounts one
// runs one replica. Two would be refused at the claim, minutes into a deploy.
func TestAVolumeAllowsOneReplica(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	seedVolume(t, st, "vol_1", "")

	rec := createService(t, h, map[string]any{
		"name": "db", "app": "shop", "replicas": 2, "volume": "vol_1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("two replicas with a volume: got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "one replica") {
		t.Errorf("the 400 does not name the rule: %s", rec.Body)
	}
	assertNoNewService(t, st)
}

// A volume already attached to a machine cannot be handed to a service: the
// claim would refuse the replica, and the machine's own snapshot names it.
func TestAnAttachedVolumeIsRefused(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	seedVolume(t, st, "vol_1", "m_1")

	rec := createService(t, h, map[string]any{
		"name": "db", "app": "shop", "replicas": 1, "volume": "vol_1",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("an attached volume: got %d, want 409: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "m_1") {
		t.Errorf("the 409 does not name the machine holding it: %s", rec.Body)
	}
	assertNoNewService(t, st)
}

// And one already bound to another service. Two services deploying onto one
// volume is the corruption the single-mounter rule exists to prevent.
func TestAVolumeMountsOneService(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	seedVolume(t, st, "vol_1", "")

	first := createService(t, h, map[string]any{
		"name": "a", "app": "shop", "replicas": 1, "volume": "vol_1",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: got %d, want 201: %s", first.Code, first.Body)
	}
	var a Service
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}

	second := createService(t, h, map[string]any{
		"name": "b", "app": "shop", "replicas": 1, "volume": "vol_1",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("second create: got %d, want 409: %s", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), a.ID) {
		t.Errorf("the 409 does not name the service already mounting it: %s", second.Body)
	}
}

// The single-mounter rule cannot be reached by the side door either. The
// create refuses more than one replica; the patch has to refuse it too, or a
// two-line script gets what the create would not give it.
func TestPatchRefusesReplicasAboveOneOnAVolumeService(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	if err := st.PutServiceVolume(context.Background(),
		&state.ServiceVolume{ServiceID: "s_1", Ordinal: 1, VolumeID: "vol_1"}); err != nil {
		t.Fatalf("PutServiceVolume: %v", err)
	}

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"replicas": 2})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patching to two replicas: got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "vol_1") {
		t.Errorf("the 400 does not name the volume: %s", rec.Body)
	}
	if row := getService(t, st); row.Replicas != 1 {
		t.Errorf("the refused patch still wrote: replicas = %d", row.Replicas)
	}

	// One and zero are both fine: a volume-backed service suspends when idle
	// exactly as any other does.
	for _, n := range []int{1, 0} {
		ok := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"replicas": n})
		if ok.Code != http.StatusOK {
			t.Errorf("patching to %d replicas: got %d, want 200: %s", n, ok.Code, ok.Body)
		}
	}
}

// The volume is create-only, and the strict decoder is what says so. A field
// that existed only to be refused would be a contract with no honest reader.
func TestPatchRefusesAVolumeByName(t *testing.T) {
	h, _ := serviceServer(t, fakeSealer{set: true})
	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"volume": "vol_2"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "volume") {
		t.Errorf("the 400 does not name the field: %s", rec.Body)
	}
}

// assertNoNewService fails when a refused create wrote a service or a
// binding: a refusal that leaves either behind is a leak the caller cannot
// see and cannot clean up.
func assertNoNewService(t *testing.T, st state.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := st.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "s_1" {
		t.Errorf("a refused create left a service row: %+v", rows)
	}
	bindings, err := st.ListServiceVolumes(ctx)
	if err != nil {
		t.Fatalf("ListServiceVolumes: %v", err)
	}
	if len(bindings) != 0 {
		t.Errorf("a refused create left a binding: %+v", bindings)
	}
}

// promoteRollout counts promotes so a test can tell a refusal from a call
// that reached the rollout.
type promoteRollout struct{ promotes int }

func (p *promoteRollout) Deploy(context.Context, string, string, json.RawMessage) (*state.Release, error) {
	return &state.Release{ID: "rel_1"}, nil
}
func (p *promoteRollout) Rollback(context.Context, string) (*state.Release, error) {
	return &state.Release{ID: "rel_1"}, nil
}
func (p *promoteRollout) Promote(context.Context, string, PromoteRequest) (*state.Service, error) {
	p.promotes++
	return &state.Service{ID: "s_1", Name: "web"}, nil
}

// promoteServer seeds one machine owned by org_1 with the given volume and
// image, and a rollout that counts what reaches it.
func promoteServer(t *testing.T, volumeID, imageRef string) (http.Handler, *promoteRollout) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")

	ctx := context.Background()
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_1", Name: "db", HostID: "host-test", State: "running",
		Domain: "db.pilotrun.app", VCPUs: 1, MemMiB: 512,
		VolumeID: volumeID, ImageRef: imageRef,
	}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_1", OrgID: "org_1", Kind: "machine"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	roll := &promoteRollout{}
	return Routes(Deps{HostID: "host-test", Store: st, Machines: newFakeManager(),
		Domain: "pilotrun.app", Rollout: roll}), roll
}

// Promote is the other door onto a volume-backed service, and both of its
// preconditions are structural: a second replica could not mount the volume,
// and a template sandbox has no image a redeploy could boot again.
func TestPromoteRefusesReplicasOnAVolumeMachine(t *testing.T) {
	h, roll := promoteServer(t, "vol_1", "bld_1")

	rec := doJSON(t, h, "POST", "/v1/machines/m_1/promote", map[string]any{"replicas": 2})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("promote with two replicas: got %d, want 400: %s", rec.Code, rec.Body)
	}
	if roll.promotes != 0 {
		t.Errorf("the refused promote reached the rollout (%d)", roll.promotes)
	}

	if ok := doJSON(t, h, "POST", "/v1/machines/m_1/promote", nil); ok.Code != http.StatusOK {
		t.Errorf("promote with no body: got %d, want 200: %s", ok.Code, ok.Body)
	}
	if roll.promotes != 1 {
		t.Errorf("the allowed promote did not reach the rollout (%d)", roll.promotes)
	}
}

func TestPromoteRefusesATemplateVolumeMachine(t *testing.T) {
	h, roll := promoteServer(t, "vol_1", "")

	rec := doJSON(t, h, "POST", "/v1/machines/m_1/promote", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("promote of a template sandbox on a volume: got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "image") {
		t.Errorf("the 400 does not say to create it from an image: %s", rec.Body)
	}
	if roll.promotes != 0 {
		t.Errorf("the refused promote reached the rollout (%d)", roll.promotes)
	}

	// Without a volume the template sandbox promotes as it always has: its
	// release is a checkpoint, and nothing about it needs an image.
	bare, bareRoll := promoteServer(t, "", "")
	if ok := doJSON(t, bare, "POST", "/v1/machines/m_1/promote", map[string]any{}); ok.Code != http.StatusOK {
		t.Errorf("promote of an ordinary template sandbox: got %d, want 200: %s", ok.Code, ok.Body)
	}
	if bareRoll.promotes != 1 {
		t.Errorf("the ordinary promote did not reach the rollout (%d)", bareRoll.promotes)
	}
}

// A service's URL follows the listener for the same reason a machine's does:
// the rig and the local box serve plain HTTP on :8080, and a link that omits
// either does not open.
func TestServiceURLFollowsTheListener(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")

	ctx := context.Background()
	if err := st.PutService(ctx, &state.Service{
		ID: "s_1", Name: "web", Replicas: 1, Domain: "web", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{
		ID: "s_1", OrgID: "org_1", Kind: "service", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	h := Routes(Deps{
		HostID: "host-test", Store: st, Domain: "pilotrun.app",
		URL: PublicURLFor(false, ":8080"),
	})

	rec := do(t, h, "GET", "/v1/services/s_1", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got Service
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URL != "http://web.pilotrun.app:8080" {
		t.Errorf("url = %q, want the listener's scheme and port", got.URL)
	}
}
