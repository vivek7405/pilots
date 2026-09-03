package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeBuilder stands in for BuildKit so the streaming contract can be tested
// without a daemon.
type fakeBuilder struct {
	lines  []BuildLogLine
	result string
	err    error

	log     []BuildLogLine
	hasLog  bool
	started int
}

func (f *fakeBuilder) NewBuildID() string { return "bld-test" }

func (f *fakeBuilder) StartBuild(_ context.Context, id string, r io.Reader,
	emit func(BuildLogLine)) (string, error) {
	f.started++
	_, _ = io.Copy(io.Discard, r)
	for _, l := range f.lines {
		emit(l)
	}
	return f.result, f.err
}

func (f *fakeBuilder) BuildLog(_ context.Context, id string, follow bool) (
	[]BuildLogLine, <-chan BuildLogLine, bool) {
	if !f.hasLog {
		return nil, nil, false
	}
	return f.log, nil, true
}

// newBuildServer wires the routes with a builder attached. The helper it
// calls seeds the store and the API key.
func newBuildServer(t *testing.T, b BuildRunner) http.Handler {
	t.Helper()
	_, st, fake := newTestServerWithManager(t)
	return Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: b})
}

func postTar(t *testing.T, h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeNDJSON(t *testing.T, body string) []BuildLogLine {
	t.Helper()
	var out []BuildLogLine
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var l BuildLogLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q is not json: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

// The gate line: POST /v1/builds streams {step, stream, line, ts} and ends
// with a usable rootfs build id.
func TestBuildStreamsNDJSONAndEndsWithABuildID(t *testing.T) {
	fb := &fakeBuilder{
		lines: []BuildLogLine{
			{Step: "[1/2] FROM alpine", Stream: "status", Line: "done", TS: 1},
			{Step: "[2/2] RUN echo hi", Stream: "stdout", Line: "hi", TS: 2},
		},
		result: "8f14e45f-ea9a-4b0c-8d9f-1c2b3a4d5e6f",
	}
	h := newBuildServer(t, fb)

	rec := postTar(t, h, "/v1/builds", []byte("tar-bytes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != ndjson {
		t.Errorf("content type is %q, want %q", ct, ndjson)
	}
	// The id is available before the body is read, so a client that loses the
	// connection can reattach without parsing anything.
	if rec.Header().Get("X-Pilot-Build-Id") != "bld-test" {
		t.Errorf("no build id header: %v", rec.Header())
	}

	lines := decodeNDJSON(t, rec.Body.String())
	if len(lines) < 4 {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	for _, l := range lines {
		if l.TS == 0 {
			t.Errorf("a line has no timestamp: %+v", l)
		}
	}
	if lines[2].Step != "[2/2] RUN echo hi" || lines[2].Line != "hi" {
		t.Errorf("the builder's lines did not reach the stream: %+v", lines)
	}

	last := lines[len(lines)-1]
	if last.Result != fb.result {
		t.Fatalf("the stream does not end with a rootfs build id: %+v", last)
	}
	if last.Error != "" {
		t.Errorf("a successful build reported an error: %+v", last)
	}
}

// The other gate line. A failed build must end the stream with a verdict and
// name the step that failed, rather than hanging or reporting success.
func TestFailedBuildEndsTheStreamWithAnError(t *testing.T) {
	fb := &fakeBuilder{
		lines: []BuildLogLine{
			{Step: "[2/2] RUN npm run build", Stream: "stderr",
				Line: "sh: vite: not found", Error: "exit code: 127", TS: 1},
		},
		err: errors.New("build failed at [2/2] RUN npm run build: exit code: 127"),
	}
	h := newBuildServer(t, fb)

	rec := postTar(t, h, "/v1/builds", []byte("tar-bytes"))
	lines := decodeNDJSON(t, rec.Body.String())

	last := lines[len(lines)-1]
	if last.Error == "" {
		t.Fatalf("a failed build ended without a verdict: %+v", last)
	}
	if last.Result != "" {
		t.Fatalf("a failed build reported a rootfs build id: %+v", last)
	}
	if !strings.Contains(last.Error, "RUN npm run build") {
		t.Errorf("the terminal line does not name the failing step: %q", last.Error)
	}

	// And the failing step's own output is in the stream, which is what an
	// agent patches its Dockerfile from.
	var sawStepOutput bool
	for _, l := range lines {
		if l.Step == "[2/2] RUN npm run build" && l.Line == "sh: vite: not found" {
			sawStepOutput = true
		}
	}
	if !sawStepOutput {
		t.Errorf("the failing step's output is missing: %+v", lines)
	}
}

// A build this host does not have is not a build with no output. A client that
// cannot tell them apart concludes the wrong thing about both.
func TestBuildLogsDistinguishMissingFromEmpty(t *testing.T) {
	missing := newBuildServer(t, &fakeBuilder{})
	if rec := do(t, missing, "GET", "/v1/builds/bld-nope/logs", testKey); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}

	empty := newBuildServer(t, &fakeBuilder{hasLog: true})
	rec := do(t, empty, "GET", "/v1/builds/bld-test/logs", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if len(decodeNDJSON(t, rec.Body.String())) != 0 {
		t.Errorf("expected an empty log, got %q", rec.Body.String())
	}
}

func TestBuildLogsReplayWhatWasRecorded(t *testing.T) {
	fb := &fakeBuilder{hasLog: true, log: []BuildLogLine{
		{Step: "s", Stream: "stdout", Line: "one", TS: 1},
		{Step: "s", Stream: "stdout", Line: "two", TS: 2},
	}}
	rec := do(t, newBuildServer(t, fb), "GET", "/v1/builds/bld-test/logs", testKey)

	lines := decodeNDJSON(t, rec.Body.String())
	if len(lines) != 2 || lines[1].Line != "two" {
		t.Fatalf("got %+v", lines)
	}
}

// A host with no object storage has nowhere to publish a build to, and says so
// rather than failing somewhere inside one.
func TestBuildRoutesReportWhenBuildsAreNotConfigured(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	noBuilds := Routes(Deps{HostID: "host-test", Store: st, Machines: fake})

	if rec := postTar(t, noBuilds, "/v1/builds", nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("POST got %d, want 501", rec.Code)
	}
	if rec := do(t, noBuilds, "GET", "/v1/builds/x/logs", testKey); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET got %d, want 501", rec.Code)
	}
}

func TestBuildRoutesRequireAuth(t *testing.T) {
	h := newBuildServer(t, &fakeBuilder{})
	if rec := do(t, h, "POST", "/v1/builds", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST got %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/builds/x/logs", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET got %d, want 401", rec.Code)
	}
}

// A build outlives the connection that started it. Killing a ten-minute build
// because a laptop closed its lid is not what anyone means by cancelling, and
// the log endpoint is how the client comes back.
func TestBuildSurvivesTheRequestContextBeingCancelled(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	fb := &cancelProbeBuilder{started: started, finished: finished}
	h := newBuildServer(t, fb)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/builds", bytes.NewReader(nil)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testKey)

	go h.ServeHTTP(httptest.NewRecorder(), req)

	<-started
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("the build was cancelled with its request")
	}
}

type cancelProbeBuilder struct {
	started, finished chan struct{}
}

func (c *cancelProbeBuilder) NewBuildID() string { return "bld-cancel" }

func (c *cancelProbeBuilder) StartBuild(ctx context.Context, _ string, _ io.Reader,
	_ func(BuildLogLine)) (string, error) {
	close(c.started)
	select {
	case <-ctx.Done():
		// Cancelled with the request, which is the bug.
	case <-time.After(500 * time.Millisecond):
		close(c.finished)
		return "ok", nil
	}
	return "", ctx.Err()
}

func (c *cancelProbeBuilder) BuildLog(context.Context, string, bool) (
	[]BuildLogLine, <-chan BuildLogLine, bool) {
	return nil, nil, false
}

// The build must be able to read its context after the stream has started.
//
// The handler streams NDJSON progress, so it writes a response before the
// build has read a byte of the upload. Go's server treats the request as
// finished once that happens, and the next read of r.Body returns
// "http: invalid Read on closed Body" -- so every build failed at its first
// instruction, with an error naming the archive rather than the cause.
//
// Driven through a real http.Server rather than a ResponseRecorder: a recorder
// has no request lifecycle and cannot reproduce this at all. That is exactly
// why the unit tests were green while nothing on the fleet could build.
func TestABuildReadsItsContextAfterStreamingBegins(t *testing.T) {
	var got []byte
	b := &readingBuilder{
		read: func(r io.Reader, emit func(BuildLogLine)) error {
			emit(BuildLogLine{Stream: "status", Line: "reading the context"})
			var err error
			got, err = io.ReadAll(r)
			return err
		},
	}

	srv := httptest.NewServer(newBuildServer(t, b))
	defer srv.Close()

	body := bytes.Repeat([]byte("context-bytes"), 1024)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/builds", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if !bytes.Equal(got, body) {
		t.Fatalf("the build read %d of %d context bytes; the stream said:\n%s",
			len(got), len(body), out)
	}
	if !bytes.Contains(out, []byte("rootfs-ok")) {
		t.Errorf("the stream carried no result:\n%s", out)
	}
}

// readingBuilder hands the context reader to a callback so a test can assert
// what the build actually managed to read.
type readingBuilder struct {
	read func(io.Reader, func(BuildLogLine)) error
}

func (b *readingBuilder) NewBuildID() string { return "bld-reading" }

func (b *readingBuilder) StartBuild(_ context.Context, _ string, r io.Reader,
	emit func(BuildLogLine)) (string, error) {
	if err := b.read(r, emit); err != nil {
		return "", err
	}
	return "rootfs-ok", nil
}

func (b *readingBuilder) BuildLog(context.Context, string, bool) (
	[]BuildLogLine, <-chan BuildLogLine, bool) {
	return nil, nil, false
}

// Concurrent builds are bounded per org on this host, and the refusal says
// "scope":"host" -- a build is not a replicated object, so there is nothing
// fleet-wide to count and the limit must not claim to be fleet-wide.
func TestConcurrentBuildsAreBoundedPerOrg(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	if err := st.PutQuota(context.Background(), &state.Quota{
		OrgID: "org_1", MaxMachines: 20, MaxVCPUs: 40, MaxMemMiB: 65536,
		MaxVolumeGiB: 100, MaxBuilds: 1,
	}); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}

	// A builder that blocks until released, so a second request really is
	// concurrent with the first rather than following it.
	release := make(chan struct{})
	inFlight := make(chan struct{})
	blocking := &blockingBuilder{release: release, inFlight: inFlight}
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake,
		Builds: blocking, BuildGate: &quota.HostGate{}})

	go func() {
		req := httptest.NewRequest("POST", "/v1/builds", bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+testKey)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-inFlight

	rec := postTar(t, h, "/v1/builds", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the second concurrent build got %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	var body QuotaExceededResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Quota != "builds" || body.Scope != "host" || body.Limit != 1 {
		t.Errorf("refusal body = %+v, want builds/host/limit 1", body)
	}

	// The slot comes back when the first build finishes.
	close(release)
	waitUntil(t, func() bool {
		return postTar(t, h, "/v1/builds", nil).Code != http.StatusTooManyRequests
	}, "the build slot to be released")
}

type blockingBuilder struct {
	release  chan struct{}
	inFlight chan struct{}
	once     sync.Once
}

func (b *blockingBuilder) NewBuildID() string { return "bld-block" }

func (b *blockingBuilder) StartBuild(_ context.Context, _ string, r io.Reader,
	_ func(BuildLogLine)) (string, error) {
	_, _ = io.Copy(io.Discard, r)
	b.once.Do(func() { close(b.inFlight) })
	<-b.release
	return "rootfs-1", nil
}

func (b *blockingBuilder) BuildLog(context.Context, string, bool) ([]BuildLogLine, <-chan BuildLogLine, bool) {
	return nil, nil, false
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
