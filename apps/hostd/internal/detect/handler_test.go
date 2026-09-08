package detect

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/compose"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

func TestTheHandlerPlansATarredRecipe(t *testing.T) {
	rec := post(t, tarOf(t, filepath.Join(fixtures, "webjs")), "?app=fx")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got compose.PlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v (%s)", err, rec.Body.String())
	}
	if got.Plan.App != "fx" {
		t.Errorf("app = %q, want fx: ?app= wins over package.json", got.Plan.App)
	}
	if len(got.Detected) != 1 || got.Detected[0].Source != "recipe" ||
		got.Detected[0].Framework != "webjs" {
		t.Fatalf("detected = %+v, want one webjs recipe", got.Detected)
	}
	if got.Detected[0].Health == nil || got.Detected[0].Health.Path != "/__webjs/ready" {
		t.Errorf("health = %+v, want the readiness path", got.Detected[0].Health)
	}
	if !strings.Contains(got.Plan.Steps[0].Dockerfile, "ENV PORT=8080") {
		t.Error("the step carries no generated Dockerfile")
	}
}

// The refusal has to be usable: an agent writes the Dockerfile from details
// alone, so the two rules and the listing travel with it.
func TestTheHandlerRefusesAnUnknownDirectoryWithEverythingNeededToFixIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "# nothing to see")
	rec := post(t, tarOf(t, dir), "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Code    string                 `json:"code"`
		Next    string                 `json:"next"`
		Details compose.UnknownDetails `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v (%s)", err, rec.Body.String())
	}
	if got.Code != "unknown_framework" {
		t.Errorf("code = %q, want unknown_framework", got.Code)
	}
	if got.Next == "" {
		t.Error("the refusal says nothing about what to do next")
	}
	if len(got.Details.Rules) != 2 {
		t.Errorf("rules = %v, want the two Dockerfile rules", got.Details.Rules)
	}
	if len(got.Details.LookedFor) != 10 {
		t.Errorf("looked_for has %d entries, want 10", len(got.Details.LookedFor))
	}
}

// The tar is untrusted. `../escape` inside one is the oldest trick there is,
// and the extractor refuses it rather than clamping it back inside the root.
func TestTheHandlerRefusesAnEscapingPath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("owned")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	rec := post(t, bytes.NewReader(buf.Bytes()), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"bad_request"`) {
		t.Errorf("body = %s, want a bad_request code", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------

func post(t *testing.T, body io.Reader, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/plan"+query, body)
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	Handler(t.TempDir(), nil, nil)(rec, req)
	return rec
}

func tarOf(t *testing.T, dir string) io.Reader {
	t.Helper()
	r, err := TarDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TarDir and ExtractContext are each other's inverse, which is what the push
// path depends on: it unpacks a GitHub tarball, writes a Dockerfile into it,
// and repacks it for the builder.
func TestTarDirRoundTrips(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a.txt", "one")
	mkdir(t, src, "sub")
	write(t, filepath.Join(src, "sub"), "b.txt", "two")

	dst := t.TempDir()
	r, err := TarDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := extractInto(r, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "one", "sub/b.txt": "two"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func extractInto(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dir, hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
}

// The context is unpacked under the root it was given, not under the process
// temp dir.
//
// os.MkdirTemp("") resolves to /tmp, which on a systemd host is very commonly
// tmpfs, so the difference is whether an authenticated caller can extract
// 2 GiB into the RAM of a host that is also running other tenants' microVMs.
//
// The staging directory is removed when the handler returns, so it can only be
// observed while the handler runs. It is observed by HOLDING the handler
// there: the request body stops on its first read, which is after the
// directory has been made and before anything has been unpacked into it.
// Sampling on a timer instead is a race, and it is one that passes on a busy
// machine and fails on a fast one.
func TestTheHandlerStagesUnderTheRootItWasGiven(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plan-work")

	body := &haltingReader{
		inner:   tarOf(t, filepath.Join(fixtures, "webjs")),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/plan", body)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Handler(root, nil, nil)(rec, req)
	}()

	<-body.started
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Errorf("nothing was staged under the root; the context went to the process temp dir: %v", err)
	} else if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "pilot-plan-") {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("staged %v under the root, want one pilot-plan-* directory", names)
	}
	close(body.release)
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// And it cleans up after itself: a plan route that left every context
	// behind would fill the cache root instead of tmpfs, which is not better.
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("the root still holds %v after the handler returned", entries)
	}
}

// haltingReader stops on its first read until it is released, so a test can
// look at the world while a handler is mid-flight.
type haltingReader struct {
	inner   io.Reader
	started chan struct{}
	release chan struct{}
	once    bool
}

func (h *haltingReader) Read(p []byte) (int, error) {
	if !h.once {
		h.once = true
		close(h.started)
		<-h.release
	}
	return h.inner.Read(p)
}

// fakeStager copies a fixture into place instead of fetching a repository, so
// the JSON branch runs with no App and no network.
type fakeStager struct {
	dir  string
	seen []string
	err  error
}

func (f *fakeStager) Stage(_ context.Context, repo, ref string) (string, error) {
	f.seen = append(f.seen, repo+"@"+ref)
	if f.err != nil {
		return "", f.err
	}
	dst, err := os.MkdirTemp("", "pilot-fake-stage-*")
	if err != nil {
		return "", err
	}
	if err := os.CopyFS(dst, os.DirFS(f.dir)); err != nil {
		return "", err
	}
	return dst, nil
}

func postRepoRef(t *testing.T, repos Stager, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/plan?app=fx", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Behind the auth middleware in production, so the tests hand the handler
	// the context it is actually given. An admin key may name any repository,
	// here as everywhere else on this API.
	req = req.WithContext(api.WithAdminPrincipal(req.Context()))
	rec := httptest.NewRecorder()
	Handler(t.TempDir(), repos, nil)(rec, req)
	return rec
}

// postRepoRefAs is the same request from a TENANT key, against a real store,
// which is the only way the connection rule can be exercised at all.
func postRepoRefAs(t *testing.T, repos Stager, st state.Store, org, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/plan?app=fx", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(api.WithTenantPrincipal(req.Context(), org))
	rec := httptest.NewRecorder()
	Handler(t.TempDir(), repos, st)(rec, req)
	return rec
}

// linkedStore is a store in which one org is connected to one repository.
func linkedStore(t *testing.T, org, repo string) state.Store {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	if err := st.PutRepoLink(context.Background(), &state.RepoLink{
		OrgID: org, Repo: repo, ConnectedAt: 1,
	}); err != nil {
		t.Fatalf("PutRepoLink: %v", err)
	}
	return st
}

// A tenant key may name a repository its org is CONNECTED to, and only that
// one. The App's installation token reaches every repository the fleet's App
// is installed on, so the repo_links row is the whole of what ties this caller
// to the one it named -- it is what replaced the admin-only gate this test
// used to assert.
func TestNamingAnUnconnectedRepositoryIsRefused(t *testing.T) {
	stager := &fakeStager{dir: filepath.Join(fixtures, "webjs")}
	st := linkedStore(t, "org_2", "acme/mine")

	rec := postRepoRefAs(t, stager, st, "org_2", `{"repo":"other/private","ref":"abc123"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"repo_not_connected"`) {
		t.Errorf("body = %s, want repo_not_connected", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/repos") {
		t.Errorf("the refusal does not say how to connect it: %s", rec.Body.String())
	}
	if len(stager.seen) != 0 {
		t.Fatalf("a refused caller reached the stager: %v", stager.seen)
	}
}

// The other half, without which the rule could be "refuse everyone" and stay
// green: connected, and the plan runs on a key carrying no admin scope at all.
//
// Spelled with different case than the row on purpose. GitHub owner and
// repository names are case-insensitive, so a caller typing Acme/Mine holds
// the same claim and must not be refused for shouting.
func TestAConnectedRepositoryPlansOnATenantKey(t *testing.T) {
	stager := &fakeStager{dir: filepath.Join(fixtures, "webjs")}
	st := linkedStore(t, "org_2", "acme/mine")

	rec := postRepoRefAs(t, stager, st, "org_2", `{"repo":"Acme/Mine","ref":"abc123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 1 {
		t.Fatalf("the stager saw %v", stager.seen)
	}
}

// One org's connection is not another's. The rows are keyed by the pair, and
// this is that decision asserted from outside the schema.
func TestAnotherOrgsConnectionDoesNotCarry(t *testing.T) {
	stager := &fakeStager{dir: filepath.Join(fixtures, "webjs")}
	st := linkedStore(t, "org_2", "acme/mine")

	rec := postRepoRefAs(t, stager, st, "org_3", `{"repo":"acme/mine","ref":"abc123"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 0 {
		t.Fatalf("another org's connection staged the repository: %v", stager.seen)
	}
}

// A repository named rather than sent is staged and planned, and the answer is
// the one a tar of the same tree produces.
func TestTheHandlerPlansAStagedRepoRef(t *testing.T) {
	stager := &fakeStager{dir: filepath.Join(fixtures, "webjs")}
	rec := postRepoRef(t, stager, `{"repo":"o/r","ref":"abc123"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 1 || stager.seen[0] != "o/r@abc123" {
		t.Fatalf("the stager saw %v", stager.seen)
	}
	var got compose.PlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v (%s)", err, rec.Body.String())
	}
	if got.Plan.App != "fx" {
		t.Errorf("app = %q, want fx: ?app= is read on this branch too", got.Plan.App)
	}
	if len(got.Detected) != 1 || got.Detected[0].Framework != "webjs" {
		t.Errorf("detected = %+v, want one webjs step", got.Detected)
	}
}

// A fleet with no App says so, and names the tar: every client that can plan
// can send one, so the refusal is actionable with no operator.
func TestAJSONBodyWithNoStagerIs503(t *testing.T) {
	rec := postRepoRef(t, nil, `{"repo":"o/r","ref":"main"}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"not_configured"`) {
		t.Errorf("body = %s, want not_configured", body)
	}
	if !strings.Contains(body, "tar") {
		t.Errorf("the next does not name the tar: %s", body)
	}
}

// Both fields are required, and an incomplete ref never reaches the fetch.
func TestAnIncompleteRepoRefIs400(t *testing.T) {
	stager := &fakeStager{dir: filepath.Join(fixtures, "webjs")}
	for _, body := range []string{`{"repo":"o/r"}`, `{"ref":"main"}`, `{}`, `not json`} {
		rec := postRepoRef(t, stager, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if len(stager.seen) != 0 {
		t.Errorf("an incomplete ref reached the stager: %v", stager.seen)
	}
}

// A failed fetch is GitHub's answer, not this host's state: 502, and the next
// says to check the App rather than to retry.
func TestAFailedFetchIsA502(t *testing.T) {
	stager := &fakeStager{err: errors.New("404 Not Found")}
	rec := postRepoRef(t, stager, `{"repo":"o/r","ref":"main"}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
		t.Errorf("body = %s, want unavailable", rec.Body.String())
	}
}

// A tar still plans. The branch is chosen by the media type, so a client that
// never learned about the JSON body is unaffected.
func TestATarStillPlansWhenAStagerIsConfigured(t *testing.T) {
	stager := &fakeStager{dir: filepath.Join(fixtures, "webjs")}
	req := httptest.NewRequest(http.MethodPost, "/v1/plan?app=fx",
		tarOf(t, filepath.Join(fixtures, "webjs")))
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	Handler(t.TempDir(), stager, nil)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 0 {
		t.Errorf("a tar upload went through the stager: %v", stager.seen)
	}
}
