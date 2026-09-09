package pilots

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient starts a server with the handler and points a client at it.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("pilot_deadbeef", WithBaseURL(srv.URL))
}

func TestBearerHeaderOnEveryCall(t *testing.T) {
	var seen string
	var body []byte
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Machine{ID: "m-1", Name: "demo", URL: "https://demo.pilotrun.app"})
	})

	m, err := c.Machines.Create(context.Background(), CreateMachineRequest{Name: "demo"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if seen != "Bearer pilot_deadbeef" {
		t.Errorf("Authorization = %q", seen)
	}
	if string(body) != `{"name":"demo"}` {
		t.Errorf("body = %q, want the name and nothing else", body)
	}
	if m.URL != "https://demo.pilotrun.app" {
		t.Errorf("url = %q", m.URL)
	}
}

func TestNotFoundIsMatchable(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"state: not found"}`))
	})

	_, err := c.Machines.Get(context.Background(), "m-nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(err, ErrNotFound) = false for %v", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Message != "state: not found" {
		t.Errorf("err = %v, want the body's message", err)
	}
}

func TestQuotaExceededCarriesTheCeiling(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota exceeded","quota":"machines","limit":20,"used":20}`))
	})

	_, err := c.Machines.Create(context.Background(), CreateMachineRequest{})
	var quota *QuotaExceeded
	if !errors.As(err, &quota) {
		t.Fatalf("errors.As(err, &QuotaExceeded) = false for %v", err)
	}
	if quota.Quota != "machines" || quota.Limit != 20 || quota.Used != 20 {
		t.Errorf("got %+v", quota)
	}
	// The underlying *Error is still reachable, so a caller that only wants
	// the status does not have to know about the subtype.
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the wrapped *Error did not survive: %v", err)
	}
}

func TestComposePlanErrorListsUnsupported(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json (never raw YAML)", ct)
		}
		var req ComposeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding the body: %v", err)
		}
		if !strings.Contains(req.Compose, "services:") || req.Env["TAG"] != "v1" {
			t.Errorf("body = %+v, want the file text and the interpolation env", req)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unsupported compose features","unsupported":` +
			`[{"service":"web","key":"privileged","message":"moot in a microVM"}]}`))
	})

	_, err := c.Compose.Plan(context.Background(), ComposeRequest{
		Compose: "services:\n  web:\n    image: nginx\n",
		Env:     map[string]string{"TAG": "v1"},
	})
	var plan *ComposePlanError
	if !errors.As(err, &plan) {
		t.Fatalf("errors.As(err, &ComposePlanError) = false for %v", err)
	}
	if len(plan.Unsupported) != 1 || plan.Unsupported[0].Key != "privileged" {
		t.Errorf("got %+v", plan.Unsupported)
	}
}

func TestPatchSendsOnlyWhatItWasGiven(t *testing.T) {
	var method, path, body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = w.Write([]byte(`{"id":"svc-1","name":"web","replicas":3}`))
	})

	replicas := 3
	if _, err := c.Services.Patch(context.Background(), "svc-1",
		UpdateServiceRequest{Replicas: &replicas}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if method != http.MethodPatch || path != "/v1/services/svc-1" {
		t.Errorf("%s %s", method, path)
	}
	if body != `{"replicas":3}` {
		t.Errorf("body = %q, want only the field that was set", body)
	}
}

func TestNoContentIsNotAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Machines.Suspend(context.Background(), "m-1"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
}

func TestBuildStreamYieldsLinesAsTheyArrive(t *testing.T) {
	released := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-tar" {
			t.Errorf("Content-Type = %q", ct)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Pilot-Build-Id", "bld-1")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		_, _ = w.Write([]byte(`{"step":"bld-1","stream":"status","line":"build accepted","ts":1}` + "\n"))
		flusher.Flush()
		// The second line is withheld until the consumer has seen the first,
		// which a buffered reader could not satisfy.
		<-released
		_, _ = w.Write([]byte(`{"step":"bld-1","stream":"status","line":"ok","result":"rootfs-xyz","ts":2}` + "\n"))
		flusher.Flush()
	})

	build, err := c.Builds.Create(context.Background(), strings.NewReader("a-tar"), BuildOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if build.ID != "bld-1" {
		t.Errorf("id = %q, want the header's value", build.ID)
	}

	var lines []string
	for line, err := range build.Lines {
		if err != nil {
			t.Fatalf("line: %v", err)
		}
		lines = append(lines, line.Line)
		if len(lines) == 1 {
			close(released)
		}
	}
	if len(lines) != 2 || lines[0] != "build accepted" {
		t.Fatalf("lines = %v", lines)
	}
}

func TestResultReadsTheLastLineAsTheVerdict(t *testing.T) {
	ndjson := func(lines ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Pilot-Build-Id", "bld-1")
			for _, l := range lines {
				_, _ = w.Write([]byte(l + "\n"))
			}
		}
	}

	t.Run("success", func(t *testing.T) {
		c := newTestClient(t, ndjson(`{"line":"step","ts":1}`, `{"line":"ok","result":"rootfs-xyz","ts":2}`))
		build, err := c.Builds.Create(context.Background(), strings.NewReader(""), BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := build.Result()
		if err != nil || got != "rootfs-xyz" {
			t.Fatalf("Result() = %q, %v", got, err)
		}
	})

	t.Run("failure under a 200", func(t *testing.T) {
		c := newTestClient(t, ndjson(`{"line":"step","ts":1}`, `{"line":"failed","error":"exit status 1","ts":2}`))
		build, err := c.Builds.Create(context.Background(), strings.NewReader(""), BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = build.Result()
		var failed *BuildFailed
		if !errors.As(err, &failed) {
			t.Fatalf("errors.As(err, &BuildFailed) = false for %v", err)
		}
		if failed.ID != "bld-1" || failed.Reason != "exit status 1" || len(failed.Lines) != 2 {
			t.Errorf("got %+v", failed)
		}
	})

	t.Run("no verdict at all", func(t *testing.T) {
		c := newTestClient(t, ndjson(`{"line":"step","ts":1}`))
		build, err := c.Builds.Create(context.Background(), strings.NewReader(""), BuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := build.Result(); err == nil {
			t.Fatal("an interrupted build read as a successful one")
		}
	})
}

func TestBaseURLPrecedence(t *testing.T) {
	t.Setenv("PILOT_API_URL", "https://host-3.example.com/")
	if got := New("k").BaseURL(); got != "https://host-3.example.com" {
		t.Errorf("PILOT_API_URL ignored: %q", got)
	}
	if got := New("k", WithBaseURL("https://host-9.example.com/")).BaseURL(); got != "https://host-9.example.com" {
		t.Errorf("WithBaseURL did not win: %q", got)
	}

	t.Setenv("PILOT_API_URL", "")
	if got := New("k").BaseURL(); got != DefaultBaseURL {
		t.Errorf("default = %q", got)
	}
}

func TestFollowLogsYieldsLines(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") != "1" {
			t.Errorf("follow = %q", r.URL.Query().Get("follow"))
		}
		_, _ = w.Write([]byte("one\ntwo\n"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := c.Machines.FollowLogs(ctx, "m-1")
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	var got []string
	for line, err := range lines {
		if err != nil {
			t.Fatalf("line: %v", err)
		}
		got = append(got, line)
	}
	if len(got) != 2 || got[0] != "one" {
		t.Errorf("lines = %v", got)
	}
}

// WithOrg reaches every route, GET and POST alike, and merges with a query the
// route already carries. A route that forgot it would create a row the same
// client's reads cannot see.
func TestWithOrgNarrowsEveryRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/services") && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode(Service{ID: "svc_1", Name: "web"})
	}))
	t.Cleanup(srv.Close)
	c := New("pilot_admin", WithBaseURL(srv.URL), WithOrg("org_2"))

	ctx := context.Background()
	if _, err := c.Services.List(ctx); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := c.Services.Create(ctx, CreateServiceRequest{Name: "web", App: "shop"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.PlanRepo(ctx, RepoRef{Repo: "o/r", Ref: "main"}, "shop"); err != nil {
		t.Fatalf("plan: %v", err)
	}

	for _, got := range seen {
		if !strings.Contains(got, "org=org_2") {
			t.Errorf("%s carries no org", got)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("saw %v, want three requests", seen)
	}
	// The plan already carried ?app=, so the org has to merge rather than
	// replace: a second "?" would make the whole query unreadable.
	if !strings.Contains(seen[2], "app=shop") || strings.Count(seen[2], "?") != 1 {
		t.Errorf("the plan's query lost a parameter: %s", seen[2])
	}
	// A client with no org sends none, so nothing changes for a tenant key.
	plain := New("pilot_x", WithBaseURL(srv.URL))
	seen = nil
	if _, err := plain.Services.List(ctx); err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(seen[0], "org=") {
		t.Errorf("a client with no org still sent one: %s", seen[0])
	}
}

// CreateFromRepo posts the repository as JSON and reads the build id out of
// the header, exactly as an uploaded context does.
func TestCreateFromRepoPostsTheRefAsJSON(t *testing.T) {
	var body []byte
	var ctype, uri string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		ctype = r.Header.Get("Content-Type")
		uri = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Pilot-Build-Id", "bld_1")
		_, _ = w.Write([]byte(`{"step":"bld_1","stream":"status","line":"build succeeded","result":"rootfs_1"}` + "\n"))
	})

	bs, err := c.Builds.CreateFromRepo(context.Background(), RepoRef{Repo: "o/r", Ref: "abc123"}, BuildOptions{App: "shop"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if bs.ID != "bld_1" {
		t.Errorf("id = %q, want bld_1", bs.ID)
	}
	if ctype != "application/json" {
		t.Errorf("content-type = %q", ctype)
	}
	if string(body) != `{"repo":"o/r","ref":"abc123"}` {
		t.Errorf("body = %q", body)
	}
	if uri != "/v1/builds?app=shop" {
		t.Errorf("uri = %q", uri)
	}
	got, err := bs.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if got != "rootfs_1" {
		t.Errorf("result = %q, want rootfs_1", got)
	}
}

// The 403 a caller gets for naming an unconnected repository says to POST
// /v1/repos. These two are what a Go caller told that can actually call, so
// the path, the method and the body are asserted rather than assumed.
func TestConnectRepoPostsTheRepositoryAndReadsItBack(t *testing.T) {
	var seenPath, seenMethod string
	var body []byte
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(RepoLinkResponse{
			Repo: "acme/shop", OrgID: "org_2", ConnectedAt: 42,
		})
	})

	link, err := c.ConnectRepo(context.Background(), "acme/shop")
	if err != nil {
		t.Fatalf("ConnectRepo: %v", err)
	}
	if seenMethod != http.MethodPost || seenPath != "/v1/repos" {
		t.Errorf("called %s %s", seenMethod, seenPath)
	}
	if string(body) != `{"repo":"acme/shop"}` {
		t.Errorf("body = %q", body)
	}
	if link.Repo != "acme/shop" || link.OrgID != "org_2" || link.ConnectedAt != 42 {
		t.Errorf("link = %+v", link)
	}
}

// Never nil on success: a caller rendering "no repositories connected" should
// not have to tell an empty fleet from a broken one.
func TestListReposUnwrapsAndIsNeverNil(t *testing.T) {
	empty := true
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/repos" || r.Method != http.MethodGet {
			t.Errorf("called %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if empty {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(RepoLinkListResponse{
			Repos: []RepoLinkResponse{{Repo: "acme/shop", OrgID: "org_2", ConnectedAt: 1}},
		})
	})

	repos, err := c.ListRepos(context.Background())
	if err != nil || repos == nil || len(repos) != 0 {
		t.Fatalf("ListRepos on an empty fleet = %#v, %v", repos, err)
	}

	empty = false
	repos, err = c.ListRepos(context.Background())
	if err != nil || len(repos) != 1 || repos[0].Repo != "acme/shop" {
		t.Fatalf("ListRepos = %#v, %v", repos, err)
	}
}
