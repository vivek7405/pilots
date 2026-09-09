package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A build that names a service deploys it ON THE HOST, once, whether or not
// anyone is still watching.
//
// The failure this replaces: the browser followed the NDJSON stream, saw the
// line carrying the image id, and posted the deploy itself. A closed tab was
// then a successful build that released nothing -- silently, with the service
// still on its old image -- and two open tabs were two rollouts of one image.

// deployingBuilder is a builder that records what it emits and lets the
// handler hold that log open past the build, which is what the real one does.
//
// The recorded log is the half that matters: a browser follows
// GET /v1/builds/{id}/logs, so a test that only read the response to the POST
// would pass while every follower saw the image and never the release.
type deployingBuilder struct {
	mu       sync.Mutex
	log      []BuildLogLine
	held     bool
	released bool
	started  chan struct{}
	// block delays the end of the build, so a test can take the watcher away
	// while it is still running.
	block chan struct{}
	// result is the rootfs build id the build produces; err fails it instead.
	result string
	err    error
	// minted counts the ids handed out, so a second build in one test gets an
	// id of its own.
	minted int
}

func newDeployingBuilder(result string) *deployingBuilder {
	return &deployingBuilder{result: result, started: make(chan struct{}, 1)}
}

// NewBuildID names the first build bld-deploy, so a test can read its log by
// name, and numbers the ones after it: two builds sharing an id would share a
// log and an owner row.
func (b *deployingBuilder) NewBuildID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.minted++
	if b.minted == 1 {
		return "bld-deploy"
	}
	return fmt.Sprintf("bld-deploy-%d", b.minted)
}

func (b *deployingBuilder) StartBuild(_ context.Context, id string, r io.Reader,
	emit func(BuildLogLine)) (string, error) {

	_, _ = io.Copy(io.Discard, r)
	select {
	case b.started <- struct{}{}:
	default:
	}
	if b.block != nil {
		<-b.block
	}
	if b.err != nil {
		return "", b.err
	}
	// The real builder appends every line to its log before it emits it.
	b.RecordLine(id, BuildLogLine{Step: id, Stream: "status", Line: "build complete",
		Result: b.result, TS: 1})
	emit(BuildLogLine{Step: id, Stream: "status", Line: "build complete",
		Result: b.result, TS: 1})
	return b.result, nil
}

func (b *deployingBuilder) BuildLog(_ context.Context, _ string, _ bool) (
	[]BuildLogLine, <-chan BuildLogLine, bool) {

	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BuildLogLine(nil), b.log...), nil, len(b.log) > 0
}

func (b *deployingBuilder) RecordRefusal(id string, line BuildLogLine) { b.RecordLine(id, line) }

func (b *deployingBuilder) HoldLog(string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held = true
}

func (b *deployingBuilder) RecordLine(_ string, line BuildLogLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, line)
}

func (b *deployingBuilder) ReleaseLog(string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released = true
}

func (b *deployingBuilder) recorded() []BuildLogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BuildLogLine(nil), b.log...)
}

func (b *deployingBuilder) state() (held, released bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held, b.released
}

func postBuild(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader([]byte("tar-bytes")))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The gate line for this route: ?deploy= cuts the release, and its id is on
// the log's last line, in the recorded log a follower reads.
func TestABuildThatNamesAServiceCutsItsRelease(t *testing.T) {
	b := newDeployingBuilder("img-1")
	roll := &recordingRollout{}
	h := deployServerWith(t, roll, b)

	rec := postBuild(t, h, "/v1/builds?deploy=svc_1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if roll.deploys != 1 {
		t.Fatalf("the release was cut %d times, want exactly 1", roll.deploys)
	}

	lines := decodeNDJSON(t, rec.Body.String())
	last := lines[len(lines)-1]
	if last.Release != "rel_1" {
		t.Errorf("the stream's last line does not carry the release: %+v", last)
	}
	// And still the image id, because a client that reads the verdict as "the
	// last line's result" -- both SDKs do -- must not break on a deploy.
	if last.Result != "img-1" {
		t.Errorf("the stream's last line lost the image id: %+v", last)
	}

	// The recorded log, which is what the browser follows. The verdict has to
	// be here and not only on the connection that started the build.
	rlines := b.recorded()
	rlast := rlines[len(rlines)-1]
	if rlast.Release != "rel_1" {
		t.Errorf("the recorded log's last line does not carry the release: %+v", rlast)
	}
	if held, released := b.state(); !held || !released {
		t.Errorf("the log was held=%v released=%v; both must be true", held, released)
	}
}

// The whole point: nobody is watching when the release is cut.
//
// Driven through a real http.Server, because a ResponseRecorder has no
// connection to close and cannot reproduce the case at all -- which is
// exactly how the browser-side deploy stayed green while a closed tab meant
// no release.
func TestTheReleaseIsCutAfterTheWatcherGoesAway(t *testing.T) {
	b := newDeployingBuilder("img-1")
	b.block = make(chan struct{})
	roll := &recordingRollout{}
	srv := httptest.NewServer(deployServerWith(t, roll, b))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/builds?deploy=svc_1",
		bytes.NewReader([]byte("tar-bytes")))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	<-b.started
	// The tab closes: the body is dropped mid-build and the connection with it.
	res.Body.Close()
	close(b.block)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if roll.deploys > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if roll.deploys != 1 {
		t.Fatalf("a build whose watcher went away cut %d releases, want exactly 1", roll.deploys)
	}
	// One rollout, and the verdict is still readable by whoever comes back.
	rlines := b.recorded()
	if last := rlines[len(rlines)-1]; last.Release != "rel_1" {
		t.Errorf("the recorded log's last line does not carry the release: %+v", last)
	}
}

// Two readers of one build are two readers, not two deploys.
func TestFollowingOneBuildTwiceCutsOneRelease(t *testing.T) {
	b := newDeployingBuilder("img-1")
	roll := &recordingRollout{}
	h := deployServerWith(t, roll, b)

	if rec := postBuild(t, h, "/v1/builds?deploy=svc_1"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 2; i++ {
		rec := do(t, h, "GET", "/v1/builds/bld-deploy/logs", testKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("reader %d: status = %d, want 200", i, rec.Code)
		}
		lines := decodeNDJSON(t, rec.Body.String())
		if last := lines[len(lines)-1]; last.Release != "rel_1" {
			t.Errorf("reader %d does not see the release: %+v", i, last)
		}
	}
	if roll.deploys != 1 {
		t.Fatalf("two readers produced %d releases, want exactly 1", roll.deploys)
	}
}

// A deploy the health gate refused reaches the reader with the ENGINE's
// words, on the log's last line: the message, the code and the next step,
// exactly what a POST to the deploy route would have answered. A build log
// that ended in "refused" and a status number would send nobody to the
// replica whose console says why.
func TestADeployTheGateRefusedEndsTheLogWithItsWords(t *testing.T) {
	b := newDeployingBuilder("img-1")
	h := deployServerWith(t, &gateFailingRollout{}, b)

	rec := postBuild(t, h, "/v1/builds?deploy=svc_1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	lines := b.recorded()
	last := lines[len(lines)-1]
	if last.Code != CodeHealthGateFailed {
		t.Fatalf("code = %q, want %q (%+v)", last.Code, CodeHealthGateFailed, last)
	}
	if !strings.Contains(last.Error, "connection refused") {
		t.Errorf("error = %q, want the gate's own message", last.Error)
	}
	if !strings.Contains(last.Next, "m_9") {
		t.Errorf("next = %q, want the replica to read the console of", last.Next)
	}
	if last.Release != "" {
		t.Errorf("a refused deploy reported a release: %+v", last)
	}
}

// A build whose deploy failed is still a build that produced an image, and
// the image id stays readable so the reader can deploy it by hand.
func TestAFailedBuildThatAskedToDeployNeverReachesTheRollout(t *testing.T) {
	b := newDeployingBuilder("img-1")
	b.err = errNoDockerfile
	roll := &recordingRollout{}
	h := deployServerWith(t, roll, b)

	rec := postBuild(t, h, "/v1/builds?deploy=svc_1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if roll.deploys != 0 {
		t.Fatalf("a failed build deployed %d times", roll.deploys)
	}
	lines := b.recorded()
	if last := lines[len(lines)-1]; last.Code != CodeBuildFailed || last.Error == "" {
		t.Errorf("the recorded log does not end on the build's failure: %+v", last)
	}
	if _, released := b.state(); !released {
		t.Error("a failed build left its log held; every follower waits forever")
	}
}

// blockingRollout stops inside Deploy until it is let go, which is what a
// rollout waiting out a health grace looks like from the outside.
type blockingRollout struct {
	recordingRollout
	entered chan struct{}
	release chan struct{}
}

func (r *blockingRollout) Deploy(ctx context.Context, serviceID, build string,
	knobs json.RawMessage) (*state.Release, error) {

	close(r.entered)
	<-r.release
	return r.recordingRollout.Deploy(ctx, serviceID, build, knobs)
}

// A rollout is not a build, and must not hold a build's slot.
//
// The gate bounds concurrent BUILDS per org on this host, and a build that
// carries a deploy now stays in the handler for the rollout as well -- a
// health grace of minutes. Held across that, the default of two slots means
// two deploying services refuse every further build with "quota exceeded,
// builds" while nothing at all is building.
func TestARolloutDoesNotHoldTheBuildsSlot(t *testing.T) {
	b := newDeployingBuilder("img-1")
	roll := &blockingRollout{entered: make(chan struct{}), release: make(chan struct{})}
	h := deployServerWith(t, roll, b)

	// One slot, so a slot held a moment too long is the difference between a
	// 200 and a 429 rather than a race nobody sees.
	if rec := doJSON(t, h, "PUT", "/v1/quotas/org_1", json.RawMessage(
		`{"max_machines":20,"max_vcpus":40,"max_mem_mib":65536,"max_volume_gib":100,"max_builds":1}`,
	)); rec.Code != http.StatusOK {
		t.Fatalf("setting the build quota: %d (%s)", rec.Code, rec.Body.String())
	}

	deploying := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/builds?deploy=svc_1", bytes.NewReader([]byte("tar-bytes")))
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/x-tar")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		deploying <- rec.Code
	}()
	<-roll.entered

	// The image exists and the rollout is waiting. The org's one build slot
	// belongs to the next build, not to this one's deploy.
	second := postBuild(t, h, "/v1/builds")
	if second.Code == http.StatusTooManyRequests {
		t.Fatalf("a rollout in flight refused the next build: %s", second.Body.String())
	}
	if second.Code != http.StatusOK {
		t.Fatalf("the second build got %d, want 200 (%s)", second.Code, second.Body.String())
	}

	close(roll.release)
	if code := <-deploying; code != http.StatusOK {
		t.Fatalf("the deploying build got %d, want 200", code)
	}
	if roll.deploys != 1 {
		t.Fatalf("the rollout ran %d times, want 1", roll.deploys)
	}
}

// A service the caller cannot reach is a 404 BEFORE the build, not a refusal
// ten minutes later. A build is minutes of a host's CPU, and whether this key
// may deploy that service is known now.
func TestABuildMayNotNameAServiceItCannotReach(t *testing.T) {
	b := newDeployingBuilder("img-1")
	roll := &recordingRollout{}
	h := deployServerWith(t, roll, b)

	rec := postBuild(t, h, "/v1/builds?deploy=svc_not_here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(b.recorded()) != 0 {
		t.Errorf("a refused build still ran: %+v", b.recorded())
	}
	if roll.deploys != 0 {
		t.Errorf("a refused build still deployed %d times", roll.deploys)
	}
	if held, _ := b.state(); held {
		t.Error("a build that never started held a log")
	}
}

// errNoDockerfile is the shape of a build failure: the builder's own message.
var errNoDockerfile = &buildError{"the build context has no Dockerfile at its root"}

type buildError struct{ msg string }

func (e *buildError) Error() string { return e.msg }
