package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// statusRecorder is a fake GitHub that keeps every commit status posted to it.
type statusRecorder struct {
	mu       sync.Mutex
	statuses []postedStatus
	fail     bool
}

type postedStatus struct {
	Repo        string
	SHA         string
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

func (r *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/access_tokens") {
		return jsonResponse(`{"token":"t"}`), nil
	}
	if idx := strings.Index(req.URL.Path, "/statuses/"); idx >= 0 {
		if r.fail {
			return &http.Response{
				StatusCode: 403, Status: "403 Forbidden",
				Body: io.NopCloser(strings.NewReader(`{"message":"Resource not accessible"}`)),
			}, nil
		}
		var p postedStatus
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &p)
		}
		p.SHA = req.URL.Path[idx+len("/statuses/"):]
		p.Repo = strings.TrimPrefix(req.URL.Path[:idx], "/repos/")
		r.mu.Lock()
		r.statuses = append(r.statuses, p)
		r.mu.Unlock()
		return jsonResponse(`{}`), nil
	}
	return jsonResponse(`{}`), nil
}

func (r *statusRecorder) all() []postedStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]postedStatus(nil), r.statuses...)
}

func statusDeps(t *testing.T, rec *statusRecorder) Deps {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	return Deps{
		HostID: "host-a",
		App: &App{
			ID: 1, PrivateKey: key, Secret: "s", BaseURL: "https://api.example",
			HTTP: &http.Client{Transport: rec},
		},
		Store:        st,
		DashboardURL: "https://pilots.run",
	}
}

// The whole point: a push that deploys says so on the commit, and the status
// links the log. Before this, the only way to learn a push had failed was to
// notice the application had not changed.
func TestAStatusCarriesTheStateContextAndLog(t *testing.T) {
	rec := &statusRecorder{}
	d := statusDeps(t, rec)

	d.status(context.Background(), "t", "gate/app", "abc1234",
		"success", "deployed api", d.buildURL("bld-7"))

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("posted %d statuses, want 1", len(got))
	}
	s := got[0]
	if s.State != "success" || s.Repo != "gate/app" || s.SHA != "abc1234" {
		t.Errorf("status = %+v", s)
	}
	if s.Context != StatusContext {
		t.Errorf("context = %q, want %q so branch protection can require it", s.Context, StatusContext)
	}
	if s.TargetURL != "https://pilots.run/builds/bld-7" {
		t.Errorf("target_url = %q, want the build's log page", s.TargetURL)
	}
	if s.Description != "deployed api" {
		t.Errorf("description = %q", s.Description)
	}
}

// A status that cannot be posted must never fail a deploy that worked. An
// installation without the statuses permission is a configuration choice, not
// a fault.
func TestAFailedStatusDoesNotFailTheDeploy(t *testing.T) {
	rec := &statusRecorder{fail: true}
	d := statusDeps(t, rec)

	// No panic, no error surfaced: status returns nothing at all.
	d.status(context.Background(), "t", "gate/app", "abc1234", "success", "deployed", "")

	if len(rec.all()) != 0 {
		t.Error("the recorder kept a status it refused")
	}
}

// GitHub caps the description at 140 characters and rejects a longer one, so a
// build error longer than that would cost the status rather than truncate it.
func TestALongDescriptionIsClipped(t *testing.T) {
	rec := &statusRecorder{}
	d := statusDeps(t, rec)

	long := "build failed: " + strings.Repeat("x", 400)
	d.status(context.Background(), "t", "gate/app", "abc1234", "failure", long, "")

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("posted %d statuses, want 1", len(got))
	}
	if n := len([]rune(got[0].Description)); n > maxStatusDescription {
		t.Errorf("description is %d characters, over GitHub's %d", n, maxStatusDescription)
	}
	if !strings.HasPrefix(got[0].Description, "build failed:") {
		t.Errorf("the clip lost the beginning of the message: %q", got[0].Description)
	}
}

// A fleet with no dashboard still posts: the state alone answers "did my push
// deploy", and a status with no link beats no status.
func TestAStatusPostsWithNoDashboard(t *testing.T) {
	rec := &statusRecorder{}
	d := statusDeps(t, rec)
	d.DashboardURL = ""

	d.status(context.Background(), "t", "gate/app", "abc1234", "success", "deployed", d.buildURL("bld-7"))

	got := rec.all()
	if len(got) != 1 || got[0].TargetURL != "" {
		t.Errorf("statuses = %+v, want one with no link", got)
	}
}

func TestStatusNeedsARepoAndASha(t *testing.T) {
	rec := &statusRecorder{}
	d := statusDeps(t, rec)

	d.status(context.Background(), "t", "gate/app", "", "success", "x", "")
	d.status(context.Background(), "t", "", "abc1234", "success", "x", "")
	d.status(context.Background(), "", "gate/app", "abc1234", "success", "x", "")

	if got := rec.all(); len(got) != 0 {
		t.Errorf("posted %+v for incomplete arguments", got)
	}
}
