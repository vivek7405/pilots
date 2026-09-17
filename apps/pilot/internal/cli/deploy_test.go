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

	pilots "github.com/vivek7405/pilots/sdks/go"
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
