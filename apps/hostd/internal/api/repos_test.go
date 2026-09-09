package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// tenantKey mints a non-admin key for an org on a test server's store.
func tenantKey(t *testing.T, st state.Store, key, org, scopes string) {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: org, Scopes: scopes,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
}

// Connecting is admin-scoped and fetching is not, and that asymmetry is the
// design: hostd cannot check from a request that an org controls a repository
// -- that proof is held at GitHub -- so the claim is asserted by a party that
// can prove it and recorded here once.
func TestConnectingARepositoryNeedsAnAdminKey(t *testing.T) {
	h, st := newTestServer(t)
	tenantKey(t, st, "pilot_deploykey", "org_2", "deploy")

	rec := postJSON(t, h, "/v1/repos", "pilot_deploykey", `{"repo":"acme/shop"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"scope_required"`) {
		t.Errorf("body = %s, want scope_required", rec.Body.String())
	}
	if links, err := st.ListRepoLinks(context.Background(), ""); err != nil || len(links) != 0 {
		t.Errorf("a refused connect wrote %v (%v)", links, err)
	}
}

// The round trip an operator or the dashboard makes: connect the repository
// for an org with ?org=, then that org's OWN key may name it.
func TestAConnectedRepositoryIsReadableByTheOrgItWasConnectedFor(t *testing.T) {
	h, st := newTestServer(t)
	tenantKey(t, st, "pilot_deploykey", "org_2", "deploy")

	rec := postJSON(t, h, "/v1/repos?org=org_2", testKey, `{"repo":"Acme/Shop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("connect: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got RepoLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	// Lowercased on the way in: GitHub names are case-insensitive, and two
	// spellings of one repository must not be two rows.
	if got.Repo != "acme/shop" || got.OrgID != "org_2" {
		t.Errorf("connected %+v, want acme/shop for org_2", got)
	}
	if got.ConnectedAt == 0 {
		t.Error("the connection carries no time")
	}

	// The tenant's own list shows it, which is what makes a refusal
	// actionable: a caller can see what it IS connected to.
	rec = do(t, h, "GET", "/v1/repos", "pilot_deploykey")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list RepoLinkListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(list.Repos) != 1 || list.Repos[0].Repo != "acme/shop" {
		t.Fatalf("the org's list is %+v", list.Repos)
	}
}

// A list is narrowed to the caller's org. Without this a tenant key would read
// which repositories every other org on the fleet deploys, which is a customer
// list.
func TestTheRepoListIsNarrowedToTheCallersOrg(t *testing.T) {
	h, st := newTestServer(t)
	tenantKey(t, st, "pilot_deploykey", "org_2", "deploy")
	for _, l := range []state.RepoLink{
		{OrgID: "org_2", Repo: "acme/mine", ConnectedAt: 1},
		{OrgID: "org_3", Repo: "other/theirs", ConnectedAt: 2},
	} {
		if err := st.PutRepoLink(context.Background(), &l); err != nil {
			t.Fatalf("PutRepoLink: %v", err)
		}
	}

	rec := do(t, h, "GET", "/v1/repos", "pilot_deploykey")
	var list RepoLinkListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(list.Repos) != 1 || list.Repos[0].Repo != "acme/mine" {
		t.Errorf("a tenant sees %+v, want only its own", list.Repos)
	}

	// An admin with no ?org= sees the fleet, as it does on every other list.
	rec = do(t, h, "GET", "/v1/repos", testKey)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(list.Repos) != 2 {
		t.Errorf("an admin sees %+v, want both", list.Repos)
	}
}

// The row is write-once, so a second connect cannot move a repository between
// orgs -- which is the whole reason ANY host is allowed to write one.
func TestConnectingTwiceCannotMoveARepositoryBetweenOrgs(t *testing.T) {
	h, st := newTestServer(t)

	if rec := postJSON(t, h, "/v1/repos?org=org_2", testKey, `{"repo":"acme/shop"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first connect: %d %s", rec.Code, rec.Body.String())
	}
	// A second connect FOR ANOTHER ORG is a different row, not a move: both
	// orgs may deploy the repository and neither displaced the other.
	if rec := postJSON(t, h, "/v1/repos?org=org_3", testKey, `{"repo":"acme/shop"}`); rec.Code != http.StatusCreated {
		t.Fatalf("second connect: %d %s", rec.Code, rec.Body.String())
	}
	for _, org := range []string{"org_2", "org_3"} {
		if _, err := st.GetRepoLink(context.Background(), org, "acme/shop"); err != nil {
			t.Errorf("%s lost its claim: %v", org, err)
		}
	}

	// And re-connecting the same pair keeps the ORIGINAL time: the write is
	// ON CONFLICT DO NOTHING, and a caller is told when the connection was
	// really made rather than when it last asked.
	first, _ := st.GetRepoLink(context.Background(), "org_2", "acme/shop")
	rec := postJSON(t, h, "/v1/repos?org=org_2", testKey, `{"repo":"acme/shop"}`)
	var again RepoLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if again.ConnectedAt != first.ConnectedAt {
		t.Errorf("connected_at moved %d -> %d on a repeat connect", first.ConnectedAt, again.ConnectedAt)
	}
}

// `repo` is compared against a string that is interpolated into GitHub API
// paths under the fleet's credential, so a row keyed by a shape the fetch
// would refuse is refused here too.
func TestConnectingARepoThatIsNotOwnerNameIs400(t *testing.T) {
	h, st := newTestServer(t)

	for _, repo := range []string{"", "owner", "owner/../app", "x/y?z", "o/r#f"} {
		rec := postJSON(t, h, "/v1/repos?org=org_2", testKey, `{"repo":"`+repo+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400: %s", repo, rec.Code, rec.Body.String())
		}
	}
	if links, err := st.ListRepoLinks(context.Background(), ""); err != nil || len(links) != 0 {
		t.Errorf("a malformed repo was recorded: %v (%v)", links, err)
	}
}

// Two orgs whose ids differ only in case must not share a claim.
//
// The row is keyed by `lower(org)/lower(repo)`, so `Org_A` and `org_a` fold to
// one id, and org ids are free-form at POST /v1/api-keys. Without comparing
// the row that came back, the second org's GetRepoLink succeeds against the
// first org's row and it inherits a claim it never made.
func TestAClaimIsNotSharedByOrgsThatDifferOnlyInCase(t *testing.T) {
	fb := &fakeBuilder{}
	stager := &fakeStager{}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	tenantKey(t, st, "pilot_upperorg", "Org_A", "deploy")
	if err := st.PutRepoLink(context.Background(), &state.RepoLink{
		OrgID: "org_a", Repo: "acme/private", ConnectedAt: 1,
	}); err != nil {
		t.Fatalf("PutRepoLink: %v", err)
	}

	rec := postJSON(t, h, "/v1/builds", "pilot_upperorg", `{"repo":"acme/private","ref":"main"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 0 {
		t.Errorf("a case-folded org id inherited another org's claim: %v", stager.seen)
	}
}

// A key whose row carries no org is handed nothing, rather than the fleet.
//
// `ListRepoLinks(ctx, "")` is the ADMIN query -- every row for every org --
// and a non-admin caller reaches it with an empty org because listOrg narrows
// to a value it does not check. No mint path writes such a key today; this is
// the same guard mayAccess takes, one route later.
func TestAnOrglessKeyIsHandedNoRepoLinks(t *testing.T) {
	h, st := newTestServer(t)
	tenantKey(t, st, "pilot_noorgkey", "", "deploy")
	for _, l := range []state.RepoLink{
		{OrgID: "org_2", Repo: "acme/mine", ConnectedAt: 1},
		{OrgID: "org_3", Repo: "other/theirs", ConnectedAt: 2},
	} {
		if err := st.PutRepoLink(context.Background(), &l); err != nil {
			t.Fatalf("PutRepoLink: %v", err)
		}
	}

	rec := do(t, h, "GET", "/v1/repos", "pilot_noorgkey")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list RepoLinkListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(list.Repos) != 0 {
		t.Errorf("a key with no org was handed %+v -- every org's connections", list.Repos)
	}
}

// `repo` is an authorization key on the service routes too: it keys the row
// AllowRepo reads, it is reflected into that refusal, and it is what
// serviceFor matches deliveries against. So its shape is checked here exactly
// as it is on the build route.
func TestAServiceRepoThatIsNotOwnerNameIs400(t *testing.T) {
	h, st := newTestServer(t)
	tenantKey(t, st, "pilot_shapekey", "org_2", "deploy")

	for _, repo := range []string{"owner", "owner/../app", "x/y?z", "owner/name/extra", "o/r#f"} {
		body := `{"name":"svc","app":"svc","replicas":1,"repo":"` + repo + `"}`
		rec := postJSON(t, h, "/v1/services", "pilot_shapekey", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create %q: got %d, want 400: %s", repo, rec.Code, rec.Body.String())
		}
	}

	// And on the patch, where the same string reaches the same places. The
	// service is created with an admin key, which needs no claim.
	created := postJSON(t, h, "/v1/services", testKey, `{"name":"patchme","app":"patchme","replicas":1}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("creating the service to patch: %d %s", created.Code, created.Body.String())
	}
	var svc Service
	if err := json.Unmarshal(created.Body.Bytes(), &svc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec := doJSON(t, h, "PATCH", "/v1/services/"+svc.ID, map[string]any{"repo": "owner/../app"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("patch: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// The disconnect still works: giving a repository up is never refused.
	if rec := doJSON(t, h, "PATCH", "/v1/services/"+svc.ID, map[string]any{"repo": ""}); rec.Code != http.StatusOK {
		t.Errorf("disconnect: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// A service is a standing order to build a repository on every push to it
// (internal/github, serviceFor), so pointing one at a repository asks the same
// question a build asks, and gets the same answer.
func TestAServiceMayOnlyNameAConnectedRepository(t *testing.T) {
	h, st := newTestServer(t)
	tenantKey(t, st, "pilot_deploykey", "org_2", "deploy")

	rec := postJSON(t, h, "/v1/services", "pilot_deploykey",
		`{"name":"hijack","app":"hijack","replicas":1,"repo":"victim/private","branch":"main","autodeploy":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"repo_not_connected"`) {
		t.Errorf("body = %s, want repo_not_connected", rec.Body.String())
	}

	// Connected, and the same create is allowed.
	if err := st.PutRepoLink(context.Background(), &state.RepoLink{
		OrgID: "org_2", Repo: "victim/private", ConnectedAt: 1,
	}); err != nil {
		t.Fatalf("PutRepoLink: %v", err)
	}
	rec = postJSON(t, h, "/v1/services", "pilot_deploykey",
		`{"name":"mine","app":"mine","replicas":1,"repo":"victim/private","branch":"main","autodeploy":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("connected: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
}
