package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pilots "github.com/pilotsrun/pilots/sdks/go"
)

// A compose build: context with no Dockerfile is planned by the host like any
// other directory, and the host's Dockerfile and readiness check reach the
// step. A context that carries its own Dockerfile is not asked about, and an
// image: step is not a context at all.
func TestABuildContextWithNoDockerfileIsPlannedByTheHost(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"web/package.json": `{"name":"web","dependencies":{"@webjsdev/core":"1"}}`,
		"api/Dockerfile":   "FROM scratch\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var asked []string
	var sawPackage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plan" {
			http.Error(w, r.URL.Path, http.StatusNotFound)
			return
		}
		asked = append(asked, r.URL.Query().Get("app"))
		sawPackage = tarHas(t, r.Body, "package.json")
		json.NewEncoder(w).Encode(pilots.ComposePlanResponse{
			Plan: pilots.ComposePlan{App: "web", Steps: []pilots.ComposeStep{{
				Name: "web", Build: &pilots.ComposeBuild{Context: "."},
				Dockerfile: "FROM node\nENV PORT=8080\n",
				Health:     &pilots.HealthCheck{Type: "http", Path: "/__webjs/ready"},
			}}},
			Detected: []pilots.ComposeDetected{{Service: "web", Source: "recipe", Framework: "webjs", Dir: "."}},
		})
	}))
	defer srv.Close()
	client := pilots.New("k", pilots.WithBaseURL(srv.URL))

	plan := pilots.ComposePlan{App: "shop", Steps: []pilots.ComposeStep{
		{Name: "web", Build: &pilots.ComposeBuild{Context: "./web"}, DockerfileAppend: "WORKDIR /app\n"},
		{Name: "api", Build: &pilots.ComposeBuild{Context: "./api"}},
		{Name: "postgres", Dockerfile: "FROM postgres:17\n"},
	}}
	if err := recogniseBuildContexts(context.Background(), client, &plan, dir, nil); err != nil {
		t.Fatalf("recogniseBuildContexts: %v", err)
	}
	if strings.Join(asked, ",") != "web" {
		t.Errorf("the host was asked about %v, want only the context with no Dockerfile", asked)
	}
	if !sawPackage {
		t.Error("the context's own files were not sent, so the host had nothing to recognise")
	}
	web := plan.Steps[0]
	if !strings.Contains(web.Dockerfile, "ENV PORT=8080") {
		t.Errorf("the host's Dockerfile did not reach the step: %q", web.Dockerfile)
	}
	if web.DockerfileAppend != "WORKDIR /app\n" {
		t.Error("the compose file's overrides were lost; they are appended at upload")
	}
	if web.Health == nil || web.Health.Path != "/__webjs/ready" {
		t.Errorf("the host's readiness check did not reach the step: %+v", web.Health)
	}
	if plan.Steps[1].Dockerfile != "" || plan.Steps[2].Dockerfile != "FROM postgres:17\n" {
		t.Errorf("steps that needed nothing were touched: %+v", plan.Steps[1:])
	}
}

// tarHas reports whether a (possibly gzipped) tar carries an entry by name.
func tarHas(t *testing.T, body io.Reader, name string) bool {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	var r io.Reader = strings.NewReader(string(raw))
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			return false
		}
		if filepath.Base(h.Name) == name {
			return true
		}
	}
}

// build: . is the common case, and then the context IS the compose project:
// the host answers with the plan of the whole file, and the step to take is
// the one with our name, not whichever service sorts first. Taking the first
// once built the web service from postgres's Dockerfile.
func TestAContextThatIsTheComposeProjectIsMatchedByName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    build: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(pilots.ComposePlanResponse{
			Plan: pilots.ComposePlan{App: "shop", Steps: []pilots.ComposeStep{
				{Name: "postgres", Dockerfile: "FROM postgres:17\n", Private: true,
					Health: &pilots.HealthCheck{Type: "process"}},
				{Name: "web", Build: &pilots.ComposeBuild{Context: "."},
					Dockerfile: "FROM node\nENV PORT=8080\n",
					Health:     &pilots.HealthCheck{Type: "http", Path: "/__webjs/ready"}},
			}},
			Detected: []pilots.ComposeDetected{
				{Service: "postgres", Source: "compose", Dir: "."},
				{Service: "web", Source: "recipe", Framework: "webjs", Dir: "."},
			},
		})
	}))
	defer srv.Close()
	client := pilots.New("k", pilots.WithBaseURL(srv.URL))

	plan := pilots.ComposePlan{App: "shop", Steps: []pilots.ComposeStep{
		{Name: "postgres", Dockerfile: "FROM postgres:17\n", Private: true, Health: &pilots.HealthCheck{Type: "process"}},
		{Name: "web", Build: &pilots.ComposeBuild{Context: "."}},
	}}
	if err := recogniseBuildContexts(context.Background(), client, &plan, dir, nil); err != nil {
		t.Fatalf("recogniseBuildContexts: %v", err)
	}
	web := plan.Steps[1]
	if !strings.Contains(web.Dockerfile, "FROM node") {
		t.Errorf("web got %q, want its own step's Dockerfile", web.Dockerfile)
	}
	if web.Health == nil || web.Health.Type != "http" {
		t.Errorf("web got %+v, want its own step's health", web.Health)
	}
}

// --app must reach the host BEFORE it plans. A compose file with no name is
// refused at planning, so a flag applied to the returned plan never runs.
func TestAppFlagNamesTheComposeProjectBeforeThePlan(t *testing.T) {
	got := withProjectName(nil, "shop")
	if got["COMPOSE_PROJECT_NAME"] != "shop" {
		t.Fatalf("--app shop sent %v to the planner; a nameless compose file is "+
			"refused before --app is ever applied", got)
	}
	// compose's own override wins over the flag that borrows it.
	explicit := map[string]string{"COMPOSE_PROJECT_NAME": "store", "TAG": "v1"}
	got = withProjectName(explicit, "shop")
	if got["COMPOSE_PROJECT_NAME"] != "store" || got["TAG"] != "v1" {
		t.Fatalf("an explicit project name was overwritten: %v", got)
	}
	// No flag, nothing added: the host derives the name as before.
	if got := withProjectName(map[string]string{"TAG": "v1"}, ""); len(got) != 1 {
		t.Fatalf("with no --app the environment changed: %v", got)
	}
	// The caller's map is never written to.
	in := map[string]string{"TAG": "v1"}
	withProjectName(in, "shop")
	if _, leaked := in["COMPOSE_PROJECT_NAME"]; leaked {
		t.Fatal("withProjectName wrote into the map it was given")
	}
}
