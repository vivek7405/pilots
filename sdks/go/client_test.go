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

	build, err := c.Builds.Create(context.Background(), strings.NewReader("a-tar"))
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
		build, err := c.Builds.Create(context.Background(), strings.NewReader(""))
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
		build, err := c.Builds.Create(context.Background(), strings.NewReader(""))
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
		build, err := c.Builds.Create(context.Background(), strings.NewReader(""))
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
