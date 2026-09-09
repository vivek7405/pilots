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

	"crypto/sha256"
	"encoding/hex"
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

	// recordEmitted mirrors the real builder, which appends a line to its log
	// store before it emits. Opt-in, so the tests that hand BuildLog a fixed
	// log keep getting exactly that log.
	recordEmitted bool
}

func (f *fakeBuilder) NewBuildID() string { return "bld-test" }

func (f *fakeBuilder) StartBuild(_ context.Context, id string, r io.Reader,
	emit func(BuildLogLine)) (string, error) {
	f.started++
	_, _ = io.Copy(io.Discard, r)
	for _, l := range f.lines {
		if f.recordEmitted {
			f.log = append(f.log, l)
			f.hasLog = true
		}
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

func (f *fakeBuilder) RecordRefusal(_ string, line BuildLogLine) {
	f.log = append(f.log, line)
	f.hasLog = true
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
	return postTarAs(t, h, path, testKey, body)
}

// postTarAs builds as a named key. The admin key passes every ownership check
// by short-circuit, so a test about who owns an image has to speak as a
// scoped key or it asserts nothing.
func postTarAs(t *testing.T, h http.Handler, path, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
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

func (c *cancelProbeBuilder) RecordRefusal(string, BuildLogLine) {}

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

// A build has two ids and they are different objects. The job id rides in
// X-Pilot-Build-Id and scopes the log route; the ROOTFS build id, minted
// inside the builder, is the one a deploy and a create name, and it is the
// one every consumer of the stream carries forward. If only the job id gets
// an owner row, a key that is not admin can build and can never deploy: the
// ownership check on the id it was handed finds nothing and answers 404.
//
// So this is the whole path a real user walks, spoken by a `deploy` key
// rather than the bootstrap admin one, which passes every check by
// short-circuit and is what hid this.
func TestAScopedKeyDeploysTheImageItBuilt(t *testing.T) {
	const image = "9d1729d5-bd7a-441b-8107-b5db4947c762"
	_, st, fake := newTestServerWithManager(t)
	ctx := context.Background()

	for _, s := range []struct{ id, org string }{{"svc_1", "org_1"}, {"svc_2", "org_2"}} {
		if err := st.PutService(ctx, &state.Service{
			ID: s.id, Name: s.id, Replicas: 1, ReleaseID: "rel_0",
		}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
		if err := st.PutTenancy(ctx, &state.Tenancy{ID: s.id, OrgID: s.org, Kind: "service"}); err != nil {
			t.Fatalf("PutTenancy: %v", err)
		}
	}
	seedKey(t, st, "pilot_org1_deploy", "org_1", "deploy")
	seedKey(t, st, "pilot_org2_deploy", "org_2", "deploy")

	// The real builder emits the rootfs id on its own "build complete" line
	// before StartBuild returns, so the fake does too. The owner has to be
	// recorded before THAT line is encoded, not after the call comes back.
	fb := &fakeBuilder{
		lines: []BuildLogLine{
			{Step: "[1/1] FROM alpine", Stream: "status", Line: "done", TS: 1},
			{Step: "build complete", Stream: "status", Line: "done", Result: image, TS: 2},
		},
		result: image,
	}
	roll := &recordingRollout{}
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Rollout: roll})

	rec := postTarAs(t, h, "/v1/builds", "pilot_org1_deploy", []byte("tar-bytes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("build: got %d (%s)", rec.Code, rec.Body.String())
	}
	lines := decodeNDJSON(t, rec.Body.String())
	if len(lines) == 0 {
		t.Fatal("the build streamed nothing")
	}
	if last := lines[len(lines)-1]; last.Result != image {
		t.Fatalf("the stream does not end with the rootfs build id: %+v", last)
	}
	for _, l := range lines {
		if l.Error != "" {
			t.Fatalf("a successful build reported an error: %+v", l)
		}
	}

	// The image's own owner row. This is what a deploy and a create check.
	own, err := st.GetTenancy(ctx, image)
	if err != nil {
		t.Fatalf("the rootfs build id has no owner row: %v", err)
	}
	if own.OrgID != "org_1" || own.Kind != "build" {
		t.Errorf("the image's owner row = %+v, want org_1 / build", own)
	}
	// And the job's row survives untouched. It scopes the log route, and the
	// two ids are different objects with different lifetimes.
	job, err := st.GetTenancy(ctx, "bld-test")
	if err != nil || job.OrgID != "org_1" {
		t.Errorf("the build job's own row was lost: %+v (%v)", job, err)
	}

	if dep := postJSON(t, h, "/v1/services/svc_1/deploy", "pilot_org1_deploy",
		`{"build":"`+image+`"}`); dep.Code != http.StatusOK {
		t.Fatalf("deploying the image it just built: got %d, want 200 (%s)",
			dep.Code, dep.Body.String())
	}
	if roll.deploys != 1 {
		t.Fatalf("the deploy did not reach the rollout (%d deploys)", roll.deploys)
	}
	if mac := postJSON(t, h, "/v1/machines", "pilot_org1_deploy",
		`{"vcpus":1,"mem_mib":512,"image":"`+image+`"}`); mac.Code != http.StatusCreated {
		t.Errorf("booting the image it just built: got %d, want 201 (%s)",
			mac.Code, mac.Body.String())
	}

	// The other half of the row: the image belongs to ONE org.
	created := fake.created
	foreign := postJSON(t, h, "/v1/services/svc_2/deploy", "pilot_org2_deploy",
		`{"build":"`+image+`"}`)
	if foreign.Code != http.StatusNotFound ||
		!strings.Contains(foreign.Body.String(), "build not found") {
		t.Errorf("deploying another org's image: got %d (%s), want 404 build not found",
			foreign.Code, foreign.Body.String())
	}
	if roll.deploys != 1 {
		t.Errorf("a foreign image reached the rollout (%d deploys)", roll.deploys)
	}
	if boot := postJSON(t, h, "/v1/machines", "pilot_org2_deploy",
		`{"vcpus":1,"mem_mib":512,"image":"`+image+`"}`); boot.Code != http.StatusNotFound {
		t.Errorf("booting another org's image: got %d, want 404 (%s)",
			boot.Code, boot.Body.String())
	}
	if fake.created != created {
		t.Errorf("the foreign create reached the manager anyway")
	}
}

// tenancyFailingStore refuses the owner row for one id and delegates every
// other write, so the build job's row still lands and only the image's fails.
type tenancyFailingStore struct {
	state.Store
	failFor string
}

func (s *tenancyFailingStore) PutTenancy(ctx context.Context, row *state.Tenancy) error {
	if row.ID == s.failFor {
		return errors.New("the store is unreachable")
	}
	return s.Store.PutTenancy(ctx, row)
}

// An id that escapes before its owner is written is an id anyone may boot, so
// if the row cannot be written the id must not leave the handler at all. The
// build still ran and the image sits orphaned in object storage, which is
// exactly what a failed upload leaves behind; what must not happen is a
// client being handed an image nobody owns.
func TestAnImageWhoseOwnerCannotBeRecordedIsNeverHandedOut(t *testing.T) {
	const image = "9d1729d5-bd7a-441b-8107-b5db4947c762"
	_, st, fake := newTestServerWithManager(t)
	ctx := context.Background()
	seedKey(t, st, "pilot_org1_deploy", "org_1", "deploy")

	fb := &fakeBuilder{
		lines: []BuildLogLine{
			{Step: "build complete", Stream: "status", Line: "done", Result: image, TS: 1},
		},
		result: image,
	}
	broken := &tenancyFailingStore{Store: st, failFor: image}
	h := Routes(Deps{HostID: "host-test", Store: broken, Machines: fake, Builds: fb})

	rec := postTarAs(t, h, "/v1/builds", "pilot_org1_deploy", []byte("tar-bytes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("build: got %d (%s)", rec.Code, rec.Body.String())
	}
	lines := decodeNDJSON(t, rec.Body.String())
	if len(lines) == 0 {
		t.Fatal("the build streamed nothing")
	}
	for _, l := range lines {
		if l.Result != "" {
			t.Fatalf("an ownerless id reached the client: %+v", l)
		}
	}
	last := lines[len(lines)-1]
	if last.Error == "" || !strings.Contains(last.Error, "owner") {
		t.Errorf("the stream did not end on the owner failure: %+v", last)
	}
	if last.Line != "build failed" {
		t.Errorf("the verdict is %q, want build failed", last.Line)
	}
	if _, err := st.GetTenancy(ctx, image); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a row was written after all: %v", err)
	}
}

// The stream's verdict and the RECORDED log's verdict are not the same thing
// when the owner row cannot be written, and the difference is worth pinning
// rather than leaving to be discovered.
//
// The builder appends every line to its log store before it emits, so the
// closure above rewrites only the streamed copy. A later GET on the log
// replays a run ending on the builder's own "build complete" line, carrying
// the id, with no failure line in it.
//
// That is safe, and it is not tidy. The log route is scoped by the job's own
// tenancy row to the org that built it, so the id reaches nobody who was not
// going to own it; and with no owner row, that org cannot use it either. The
// last assertion here is the one that matters: the id is unusable rather than
// ownerless.
func TestTheRecordedLogKeepsTheLineTheStreamRewrote(t *testing.T) {
	const image = "9d1729d5-bd7a-441b-8107-b5db4947c762"
	_, st, fake := newTestServerWithManager(t)
	seedKey(t, st, "pilot_org1_deploy", "org_1", "deploy")

	fb := &fakeBuilder{
		recordEmitted: true,
		lines: []BuildLogLine{
			{Step: "build complete", Stream: "status", Line: "done", Result: image, TS: 1},
		},
		result: image,
	}
	broken := &tenancyFailingStore{Store: st, failFor: image}
	h := Routes(Deps{HostID: "host-test", Store: broken, Machines: fake, Builds: fb})

	rec := postTarAs(t, h, "/v1/builds", "pilot_org1_deploy", []byte("tar-bytes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("build: got %d (%s)", rec.Code, rec.Body.String())
	}
	for _, l := range decodeNDJSON(t, rec.Body.String()) {
		if l.Result != "" {
			t.Fatalf("the STREAM handed out an ownerless id: %+v", l)
		}
	}

	// The recorded copy still carries it. This is the divergence, asserted so
	// that the comment above the closure stays honest.
	replay := do(t, h, "GET", "/v1/builds/bld-test/logs", "pilot_org1_deploy")
	if replay.Code != http.StatusOK {
		t.Fatalf("replaying the log: got %d (%s)", replay.Code, replay.Body.String())
	}
	var carried bool
	for _, l := range decodeNDJSON(t, replay.Body.String()) {
		if l.Result == image {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the recorded log no longer carries the id; the comment on "+
			"the write closure needs updating: %s", replay.Body.String())
	}

	// And the assertion the narrowed claim rests on: reading it changes
	// nothing, because without an owner row it boots nowhere.
	if boot := postJSON(t, h, "/v1/machines", "pilot_org1_deploy",
		`{"vcpus":1,"mem_mib":512,"image":"`+image+`"}`); boot.Code != http.StatusNotFound {
		t.Errorf("an id with no owner row was bootable by the org that built "+
			"it: got %d (%s)", boot.Code, boot.Body.String())
	}
}

// flakyTenancyStore lets the first owner write for an id land and fails every
// one after it. That is the shape of a store blip between the builder's own
// completion line and the handler's terminal line, both of which carry the id.
type flakyTenancyStore struct {
	state.Store
	forID string
	calls int
}

func (s *flakyTenancyStore) PutTenancy(ctx context.Context, row *state.Tenancy) error {
	if row.ID == s.forID {
		s.calls++
		if s.calls > 1 {
			return errors.New("the store is unreachable")
		}
	}
	return s.Store.PutTenancy(ctx, row)
}

// Two lines carry the rootfs build id, so the owner row must be written on the
// first and not attempted again on the second.
//
// The store is write-once, so a second call could not corrupt anything, but it
// is a round trip that can fail on its own. Tracking "has not failed yet"
// rather than "has already succeeded" meant a blip on that second call turned
// a build that had SUCCEEDED, whose image was published and whose owner row
// was already correct, into a reported failure the client throws away.
func TestASecondOwnerWriteNeverFailsABuildThatSucceeded(t *testing.T) {
	const image = "9d1729d5-bd7a-441b-8107-b5db4947c762"
	_, st, fake := newTestServerWithManager(t)
	ctx := context.Background()
	seedKey(t, st, "pilot_org1_deploy", "org_1", "deploy")

	fb := &fakeBuilder{
		lines: []BuildLogLine{
			{Step: "build complete", Stream: "status", Line: "done", Result: image, TS: 1},
		},
		result: image,
	}
	flaky := &flakyTenancyStore{Store: st, forID: image}
	h := Routes(Deps{HostID: "host-test", Store: flaky, Machines: fake, Builds: fb})

	rec := postTarAs(t, h, "/v1/builds", "pilot_org1_deploy", []byte("tar-bytes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("build: got %d (%s)", rec.Code, rec.Body.String())
	}
	lines := decodeNDJSON(t, rec.Body.String())
	if len(lines) == 0 {
		t.Fatal("the build streamed nothing")
	}
	last := lines[len(lines)-1]
	if last.Error != "" {
		t.Fatalf("a build that succeeded was reported as failed: %+v", last)
	}
	if last.Result != image {
		t.Fatalf("the stream does not end with the rootfs build id: %+v", last)
	}

	// The direct statement of it: one line wrote the row, the other did not
	// try. This is what fails if the flag goes back to reading "not yet
	// failed" instead of "already succeeded".
	if flaky.calls != 1 {
		t.Errorf("the owner row was written %d times, want exactly 1", flaky.calls)
	}

	own, err := st.GetTenancy(ctx, image)
	if err != nil {
		t.Fatalf("the rootfs build id has no owner row: %v", err)
	}
	if own.OrgID != "org_1" {
		t.Errorf("the image's owner row = %+v, want org_1", own)
	}
	// And the image the client was handed actually works.
	if boot := postJSON(t, h, "/v1/machines", "pilot_org1_deploy",
		`{"vcpus":1,"mem_mib":512,"image":"`+image+`"}`); boot.Code != http.StatusCreated {
		t.Errorf("booting the image it built: got %d, want 201 (%s)",
			boot.Code, boot.Body.String())
	}
}

func (x *readingBuilder) RecordRefusal(string, BuildLogLine) {}

func (x *blockingBuilder) RecordRefusal(string, BuildLogLine) {}

// fakeStager stands in for the fetch and plan internal/github does, so the
// route's branch can be tested with no App, no network and no repository.
type fakeStager struct {
	seen []string
	tar  string
	err  error
}

func (f *fakeStager) Context(_ context.Context, id, repo, ref, app string) (io.ReadCloser, error) {
	f.seen = append(f.seen, id+" "+repo+"@"+ref+" app="+app)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.tar)), nil
}

// A repository named rather than uploaded reaches the builder through the
// stager, and the stream is the one an uploaded tar produces.
func TestABuildFromARepoRefUsesTheStager(t *testing.T) {
	fb := &fakeBuilder{
		lines:  []BuildLogLine{{Step: "[1/1] FROM alpine", Stream: "status", Line: "done", TS: 1}},
		result: "rootfs-1",
	}
	stager := &fakeStager{tar: "tar-bytes"}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	rec := postJSON(t, h, "/v1/builds?app=shop", testKey, `{"repo":"o/r","ref":"abc123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Pilot-Build-Id") != "bld-test" {
		t.Errorf("no build id in the header: %v", rec.Header())
	}
	if len(stager.seen) != 1 || stager.seen[0] != "bld-test o/r@abc123 app=shop" {
		t.Fatalf("the stager saw %v", stager.seen)
	}
	if fb.started != 1 {
		t.Errorf("the builder ran %d times, want once", fb.started)
	}
	lines := decodeNDJSON(t, rec.Body.String())
	if len(lines) == 0 || lines[len(lines)-1].Result != "rootfs-1" {
		t.Errorf("the stream does not end with the image id: %v", lines)
	}
	// The job's owner is recorded before the id leaves the handler, or the
	// log route would 404 to the org that asked for the build.
	if row, err := st.GetTenancy(context.Background(), "bld-test"); err != nil || row.OrgID != "org_1" {
		t.Errorf("tenancy = %+v, %v; want org_1", row, err)
	}
}

// A fleet with no App says so, and says what to send instead. 503 rather than
// a 501: the route works on a fleet whose hosts carry an App.
func TestARepoRefWithNoStagerIs503(t *testing.T) {
	fb := &fakeBuilder{result: "rootfs-1"}
	h := newBuildServer(t, fb)

	rec := postJSON(t, h, "/v1/builds", testKey, `{"repo":"o/r","ref":"main"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"not_configured"`) {
		t.Errorf("body = %s, want not_configured", body)
	}
	// The next has to name the tar, or a caller with the bytes in hand is told
	// only to go and find an operator.
	if !strings.Contains(body, "tar") {
		t.Errorf("the next does not name the tar: %s", body)
	}
	if fb.started != 0 {
		t.Errorf("the builder ran on a fleet with no App")
	}
}

// A refusal is the planner's verdict on the repository, so it is a 400 with
// the planner's own code, and the reason is readable at the build's log the
// way a push's refusal is.
func TestARefusedRepoRefIsA400AndReadableAtTheLog(t *testing.T) {
	fb := &fakeBuilder{}
	stager := &fakeStager{err: &Refusal{
		BuildID: "bld-test", Code: CodePlanMultiService,
		Message: "the plan has 2 services; a push deploys one",
		Next:    "commit a compose file and deploy it with pilot deploy",
	}}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	rec := postJSON(t, h, "/v1/builds", testKey, `{"repo":"o/r","ref":"main"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"plan_multi_service"`) {
		t.Errorf("body = %s, want plan_multi_service", rec.Body.String())
	}
	// The 400 carries the id in the header: the body names no build, and the
	// log the refusal was recorded under is only reachable through it. This
	// used to be asserted by the fleet gate alone (gate.sh 21b).
	if got := rec.Header().Get("X-Pilot-Build-Id"); got != "bld-test" {
		t.Errorf("X-Pilot-Build-Id = %q, want bld-test", got)
	}
	if fb.started != 0 {
		t.Errorf("a refused plan reached the builder")
	}
	// The owner row is written before the fetch, so the refusal the stager
	// recorded is readable by the org it was for rather than 404.
	if row, err := st.GetTenancy(context.Background(), "bld-test"); err != nil || row.OrgID != "org_1" {
		t.Errorf("tenancy = %+v, %v; want org_1", row, err)
	}
}

// Both fields are required. An empty ref would fetch the default branch of
// whatever the repository is now, which is not what any caller means.
func TestARepoRefWithAnEmptyFieldIs400(t *testing.T) {
	stager := &fakeStager{tar: "tar-bytes"}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake,
		Builds: &fakeBuilder{result: "r"}, Repos: stager})

	for _, body := range []string{`{"repo":"o/r"}`, `{"ref":"main"}`, `{}`} {
		rec := postJSON(t, h, "/v1/builds", testKey, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if len(stager.seen) != 0 {
		t.Errorf("an incomplete ref reached the stager: %v", stager.seen)
	}
}

// A tar still builds. The branch is chosen by the media type, so a client that
// never learned about the JSON body is unaffected.
func TestATarStillBuildsWhenAStagerIsConfigured(t *testing.T) {
	fb := &fakeBuilder{result: "rootfs-1"}
	stager := &fakeStager{tar: "never"}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	rec := postTar(t, h, "/v1/builds", []byte("tar-bytes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 0 {
		t.Errorf("a tar upload went through the stager: %v", stager.seen)
	}
	if fb.started != 1 {
		t.Errorf("the builder ran %d times, want once", fb.started)
	}
}

// A tenant key may not name a repository.
//
// The App's installation token reaches every repository the fleet's App is
// installed on, and nothing here ties the caller's org to the one it named, so
// a tenant naming another tenant's private repository would read its source
// through the fleet's own credential and then exec into the image. A push
// cannot do that: it is a signed delivery about a repository, resolved to the
// service rows that name it.
//
// Sending a tar is unaffected, which is the point: the gate is on naming, not
// on building.
// The rule that replaced the admin-only gate: a tenant key may name a
// repository its org is CONNECTED to, and no other.
//
// This test used to assert the gate PR #88 put here -- "naming a repository
// needs an admin key" -- which shut the hole by making the whole {repo, ref}
// path operator-only. The hole is the same one: the App's installation token
// reaches every repository the fleet's App is installed on, so without a claim
// a tenant could build another tenant's private source and exec into the
// image. What changed is that the claim is now a row rather than a scope.
func TestNamingARepositoryNeedsAClaimOnIt(t *testing.T) {
	fb := &fakeBuilder{}
	stager := &fakeStager{}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	const tenant = "pilot_tenantkey"
	sum := sha256.Sum256([]byte(tenant))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_2", Scopes: "deploy",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	// No claim on record: refused, with the code and the next step a client
	// can act on. Not scope_required -- holding a wider key is not the remedy.
	rec := postJSON(t, h, "/v1/builds", tenant, `{"repo":"acme/private","ref":"main"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unconnected: got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"repo_not_connected"`) {
		t.Errorf("unconnected: body = %s, want repo_not_connected", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/repos") {
		t.Errorf("the refusal does not say how to connect it: %s", rec.Body.String())
	}
	if fb.started != 0 {
		t.Errorf("a refused caller reached the builder")
	}
	// And nothing about the refusal is recorded: a tenancy row gossips to
	// every host, and a client retrying a repository it may not have would
	// otherwise write one per attempt.
	if n := countTenancy(t, st); n != 0 {
		t.Errorf("a refused build wrote %d tenancy rows", n)
	}

	// Connected, by the same org, and the build runs on a key with no admin
	// scope at all -- which is the product change this row buys.
	if err := st.PutRepoLink(context.Background(), &state.RepoLink{
		OrgID: "org_2", Repo: "acme/private", ConnectedAt: 1,
	}); err != nil {
		t.Fatalf("PutRepoLink: %v", err)
	}
	rec = postJSON(t, h, "/v1/builds", tenant, `{"repo":"acme/private","ref":"main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("connected: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 1 {
		t.Errorf("the connected build did not reach the stager: %v", stager.seen)
	}
}

// Another org's connection is not this org's. The rows are keyed by the pair
// precisely so that a repository can be connected twice without either claim
// leaking into the other.
func TestOneOrgsRepoClaimDoesNotCarryToAnother(t *testing.T) {
	fb := &fakeBuilder{}
	stager := &fakeStager{}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	const tenant = "pilot_othertenant"
	sum := sha256.Sum256([]byte(tenant))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_3", Scopes: "deploy",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	if err := st.PutRepoLink(context.Background(), &state.RepoLink{
		OrgID: "org_2", Repo: "acme/private", ConnectedAt: 1,
	}); err != nil {
		t.Fatalf("PutRepoLink: %v", err)
	}

	rec := postJSON(t, h, "/v1/builds", tenant, `{"repo":"acme/private","ref":"main"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 0 {
		t.Errorf("another org's claim staged the repository: %v", stager.seen)
	}
}

// An admin key keeps working across orgs, here as everywhere else on this API.
// The fleet's own tooling holds one, so a change that broke this would take
// the dashboard and `pilot deploy` with it.
func TestAnAdminKeyStillNamesAnyRepository(t *testing.T) {
	fb := &fakeBuilder{}
	stager := &fakeStager{}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	rec := postJSON(t, h, "/v1/builds", testKey, `{"repo":"acme/nobody-connected","ref":"main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(stager.seen) != 1 {
		t.Errorf("the admin build did not reach the stager: %v", stager.seen)
	}
}

// `repo` is interpolated into GitHub API paths under the fleet's App
// credential, so the API validates its shape rather than trusting a client to.
func TestARepoThatIsNotOwnerNameIs400(t *testing.T) {
	fb := &fakeBuilder{}
	stager := &fakeStager{}
	_, st, fake := newTestServerWithManager(t)
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb, Repos: stager})

	for _, repo := range []string{"owner/../app", "x/y?z", "owner", "owner/name/extra", "o/r#f"} {
		rec := postJSON(t, h, "/v1/builds", testKey, `{"repo":"`+repo+`","ref":"main"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400: %s", repo, rec.Code, rec.Body.String())
		}
	}
	if len(stager.seen) != 0 {
		t.Errorf("a malformed repo reached the stager: %v", stager.seen)
	}
}

// A refusal that happens before the build has an owner must not leave a
// tenancy row: those rows gossip to every host, and a client retrying a bad
// body on a fleet with no App would write one per attempt.
func TestARefusalBeforeTheOwnerRowWritesNoTenancy(t *testing.T) {
	fb := &fakeBuilder{}
	_, st, fake := newTestServerWithManager(t)
	// No Repos: this fleet has no App, which is the 503 that used to land
	// after the row was written.
	h := Routes(Deps{HostID: "host-test", Store: st, Machines: fake, Builds: fb})

	before := countTenancy(t, st)
	rec := postJSON(t, h, "/v1/builds", testKey, `{"repo":"o/r","ref":"main"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if after := countTenancy(t, st); after != before {
		t.Errorf("tenancy rows %d -> %d; a refusal recorded an owner", before, after)
	}
}

func countTenancy(t *testing.T, st state.Store) int {
	t.Helper()
	rows, err := st.ListTenancy(context.Background())
	if err != nil {
		t.Fatalf("ListTenancy: %v", err)
	}
	return len(rows)
}
