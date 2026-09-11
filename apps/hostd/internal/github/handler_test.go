package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A push to a repository with no Dockerfile builds, because the planner writes
// one from the recipe before the builder sees the context.
//
// This is the whole point of planning before building: the builder refuses a
// context with no Dockerfile, so before this a connected repository that had
// never had one simply could not be deployed by a push.
func TestAPushToARepoWithNoDockerfileIsPlannedAndBuilt(t *testing.T) {
	builds := &recordingBuilds{}
	d := pushDeps(t, tarballOf(t, "webjs"), builds)

	rootfs, step, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", "org_1")
	if err != nil {
		t.Fatalf("buildRef: %v", err)
	}
	if rootfs != "rootfs_1" {
		t.Errorf("rootfs = %q, want rootfs_1", rootfs)
	}
	if step == nil || step.Health == nil || step.Health.Path != "/__webjs/ready" {
		t.Fatalf("step = %+v, want the recipe's health", step)
	}
	// The generated Dockerfile reached the build context, which is the only
	// way the builder can turn this repository into a rootfs at all.
	if !strings.Contains(builds.context, "ENV PORT=8080") {
		t.Error("the recipe's Dockerfile is not in the tar the builder was handed")
	}
	if !strings.Contains(builds.context, "package.json") {
		t.Error("the repository's own files are not in the tar")
	}
}

// A repository whose own Dockerfile is built as it is. The platform's guess
// never overrides the author's file.
func TestAPushToARepoWithADockerfileBuildsItUnchanged(t *testing.T) {
	dir := t.TempDir()
	const dockerfile = "FROM alpine:3.20\nCMD [\"sh\", \"-c\", \"sleep 1\"]\n"
	writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
	writeFile(t, filepath.Join(dir, "package.json"), `{"dependencies":{"@webjsdev/core":"1"}}`)

	builds := &recordingBuilds{}
	d := pushDeps(t, gzipTarball(t, dir), builds)

	if _, _, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", "org_1"); err != nil {
		t.Fatalf("buildRef: %v", err)
	}
	if !strings.Contains(builds.context, dockerfile) {
		t.Errorf("the repository's own Dockerfile is not what was built:\n%s", builds.context)
	}
	if strings.Contains(builds.context, "ENV PORT=8080") {
		t.Error("a recipe was generated over an existing Dockerfile")
	}
}

// A monorepo without a compose file is refused, and the refusal is readable
// where a failed build's log is. Executing a multi-service plan inside hostd
// would be a second executor beside the CLI's.
func TestAMultiServicePushIsRefusedAndRecorded(t *testing.T) {
	builds := &recordingBuilds{}
	d := pushDeps(t, tarballOf(t, "..", "workspace-app"), builds)

	_, _, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", "org_1")
	if err == nil {
		t.Fatal("a two-service repository was built by a push")
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T (%v), want a *Refusal", err, err)
	}
	if refusal.Code != api.CodePlanMultiService {
		t.Errorf("code = %q, want %q", refusal.Code, api.CodePlanMultiService)
	}
	if refusal.Next == "" {
		t.Error("the refusal says nothing about what to do")
	}
	if builds.started {
		t.Error("the builder ran for a plan that was refused")
	}
	// The record a person reads. Without it the only trace is a journal line
	// on whichever host happened to act.
	if len(builds.refusals) != 1 {
		t.Fatalf("%d refusal lines recorded, want 1", len(builds.refusals))
	}
	line := builds.refusals[0]
	if line.Code != api.CodePlanMultiService || line.Error == "" {
		t.Errorf("the recorded line is %+v", line)
	}
	if line.Step != refusal.BuildID {
		t.Errorf("the line is under %q, the refusal names %q", line.Step, refusal.BuildID)
	}
}

// A repository nothing recognises is refused with the code an agent branches
// on, and the next step names the fix.
func TestAnUnrecognisedPushIsRefusedWithUnknownFramework(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "# nothing here")

	builds := &recordingBuilds{}
	d := pushDeps(t, gzipTarball(t, dir), builds)

	_, _, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", "org_1")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T (%v), want a *Refusal", err, err)
	}
	if refusal.Code != api.CodeUnknownFramework {
		t.Errorf("code = %q, want %q", refusal.Code, api.CodeUnknownFramework)
	}
	if !strings.Contains(refusal.Next, "Dockerfile") {
		t.Errorf("next = %q, want it to name the fix", refusal.Next)
	}
}

// The comment a pull request gets when its preview was refused. A pull request
// has one surface, so this is where the reason has to land.
func TestARefusedPreviewCommentNamesTheReasonAndTheNextStep(t *testing.T) {
	body := refusalComment("0123456789abcdef", &Refusal{
		Code: api.CodePlanMultiService, Message: "the plan has 2 services; a push deploys one",
		Next: "commit a compose file and deploy it with pilot deploy",
	})
	for _, want := range []string{
		"`0123456`", "was not built", "the plan has 2 services", "Next: commit a compose file",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment is missing %q:\n%s", want, body)
		}
	}
}

// ---------------------------------------------------------------------------

// recordingBuilds captures what the builder was handed instead of running one.
type recordingBuilds struct {
	lastID string
	// onStart runs inside StartBuild, which is the one moment the staging
	// directory is guaranteed to exist.
	onStart  func()
	started  bool
	context  string
	refusals []api.BuildLogLine
	n        int
}

func (r *recordingBuilds) NewBuildID() string {
	r.n++
	r.lastID = fmt.Sprintf("bld_%d", r.n)
	return r.lastID
}

func (r *recordingBuilds) StartBuild(_ context.Context, _, _ string, contextTar io.Reader,
	_ func(api.BuildLogLine)) (string, error) {

	r.started = true
	if r.onStart != nil {
		r.onStart()
	}
	raw, err := io.ReadAll(contextTar)
	if err != nil {
		return "", err
	}
	r.context = string(raw)
	return "rootfs_1", nil
}

func (r *recordingBuilds) RecordRefusal(_ string, line api.BuildLogLine) {
	r.refusals = append(r.refusals, line)
}

// fakeGitHub answers the three calls the staging path makes, so it runs end to
// end with no GitHub App and no network. installations records every
// repository asked about, so a test can assert the lookup happened.
type fakeGitHub struct {
	tarball       []byte
	installations []string
}

func (f *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/installation"):
		f.installations = append(f.installations, strings.TrimSuffix(
			strings.TrimPrefix(req.URL.Path, "/repos/"), "/installation"))
		return jsonResponse(`{"id":1}`), nil
	case strings.Contains(req.URL.Path, "/access_tokens"):
		return jsonResponse(`{"token":"t"}`), nil
	case strings.Contains(req.URL.Path, "/tarball/"):
		return &http.Response{
			StatusCode: 200, Status: "200 OK",
			Body:   io.NopCloser(bytes.NewReader(f.tarball)),
			Header: http.Header{"Content-Type": []string{"application/gzip"}},
		}, nil
	}
	return jsonResponse(`{}`), nil
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200, Status: "200 OK",
		Body:   io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
}

func pushDeps(t *testing.T, tarball []byte, builds *recordingBuilds) Deps {
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
			HTTP: &http.Client{Transport: &fakeGitHub{tarball: tarball}},
		},
		Store:  st,
		Builds: builds,
	}
}

func pushEvent() Event {
	ev := Event{}
	ev.Repository.FullName = "gate/webjs-app"
	ev.Installation.ID = 1
	ev.After = "abc1234"
	return ev
}

// tarballOf wraps a detect fixture the way GitHub does: everything under one
// directory named <owner>-<repo>-<sha7>, which StripRoot removes.
func tarballOf(t *testing.T, parts ...string) []byte {
	t.Helper()
	root := filepath.Join("..", "detect", "testdata")
	if len(parts) > 1 {
		root = filepath.Join(root, parts[1])
	} else {
		root = filepath.Join(root, "frameworks", parts[0])
	}
	return gzipTarball(t, root)
}

func gzipTarball(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	const wrapper = "gate-webjs-app-abc1234"
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: wrapper + "/" + filepath.ToSlash(rel), Mode: 0o644,
			Size: int64(len(raw)), Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err = tw.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The health the planner worked out is written onto a service that has none,
// so the rollout gates on the readiness path the framework serves rather than
// on hostd's default.
func TestAPushRecordsThePlansHealthOnAServiceWithNone(t *testing.T) {
	builds := &recordingBuilds{}
	d := pushDeps(t, tarballOf(t, "webjs"), builds)
	d.Rollout = &noopRollout{}

	ctx := context.Background()
	svc := &state.Service{
		ID: "svc_1", Name: "web", App: "shop", Replicas: 1,
		Repo: "gate/webjs-app", Branch: "main", Autodeploy: true,
	}
	if err := d.Store.PutService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	ev := pushEvent()
	ev.Ref = "refs/heads/main"
	if err := d.onPush(ctx, ev); err != nil {
		t.Fatalf("onPush: %v", err)
	}

	got, err := d.Store.GetService(ctx, "svc_1")
	if err != nil {
		t.Fatal(err)
	}
	var health api.HealthCheck
	if err := json.Unmarshal([]byte(got.Health), &health); err != nil {
		t.Fatalf("health = %q: %v", got.Health, err)
	}
	if health.Path != "/__webjs/ready" {
		t.Errorf("health.path = %q, want the readiness path", health.Path)
	}
}

type noopRollout struct{}

func (noopRollout) Deploy(_ context.Context, serviceID, _ string,
	_ json.RawMessage) (*state.Release, error) {

	return &state.Release{ID: "rel_1", ServiceID: serviceID}, nil
}

// The build a push made, and the refusal a push recorded, both belong to the
// org that owns the service.
//
// GET /v1/builds/{id}/logs is scoped by the tenancy row, so without one the
// route answers 404 to every key except an admin one. The refusal would then
// be readable by nobody the push was for, which is the whole outcome the
// refusal exists to produce; the battery and the fleet gate would not have
// noticed, because both drive the API with the bootstrap admin key.
func TestAPushRecordsTheBuildsOwnerForBothOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture []string
		refused bool
	}{
		{"a build that ran", []string{"webjs"}, false},
		{"a refusal", []string{"..", "workspace-app"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builds := &recordingBuilds{}
			d := pushDeps(t, tarballOf(t, tc.fixture...), builds)
			ctx := context.Background()

			_, _, err := d.buildRef(ctx, pushEvent(), "abc1234", "shop", "org_7")
			if tc.refused {
				var refusal *Refusal
				if !errors.As(err, &refusal) {
					t.Fatalf("err = %T (%v), want a *Refusal", err, err)
				}
			} else if err != nil {
				t.Fatalf("buildRef: %v", err)
			}

			row, err := d.Store.GetTenancy(ctx, builds.lastID)
			if err != nil {
				t.Fatalf("no tenancy row for build %s: %v", builds.lastID, err)
			}
			if row.OrgID != "org_7" {
				t.Errorf("build %s belongs to %q, want org_7", builds.lastID, row.OrgID)
			}
			if row.Kind != "build" {
				t.Errorf("kind = %q, want build", row.Kind)
			}
		})
	}
}

// A service with no tenancy row predates tenancy. The push still deploys; it
// simply has no owner to record, and inventing one would give the build to an
// org that never asked for it.
func TestAPushWithNoOwningOrgStillBuilds(t *testing.T) {
	builds := &recordingBuilds{}
	d := pushDeps(t, tarballOf(t, "webjs"), builds)

	if _, _, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", ""); err != nil {
		t.Fatalf("buildRef: %v", err)
	}
	if !builds.started {
		t.Error("the build did not run")
	}
}

// A service that names a repository its org holds no claim on is LOGGED and
// still built, and both halves of that are the decision.
//
// `services.repo` was ungated before repo_links existed, so every row written
// until then is grandfathered: it keeps its standing order to build that
// repository on every commit, and the checks on the create and the patch close
// the door only for rows written from now on. Refusing them instead would stop
// every autodeploy on the fleet the moment a host upgrades, since no existing
// row has a claim, so the fleet keeps building them and names each one once
// per push for an operator to reconcile.
//
// Counterfactual: make warnUnclaimed refuse, and "still built" fails; drop it,
// and the fleet builds a grandfathered row with nothing anywhere saying so.
func TestAnUnclaimedRepositoryIsLoggedAndStillBuilt(t *testing.T) {
	builds := &recordingBuilds{}
	d := pushDeps(t, tarballOf(t, "webjs"), builds)
	svc := &state.Service{ID: "svc_1", Repo: "gate/webjs-app", Autodeploy: true}

	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	d.warnUnclaimed(context.Background(), svc, "org_1", "gate/webjs-app")
	if !strings.Contains(logged.String(), "svc_1") || !strings.Contains(logged.String(), "gate/webjs-app") {
		t.Errorf("the warning does not name the service and the repository: %s", logged.String())
	}

	// The build runs regardless: this path is a signed delivery, not a request
	// from a tenant, and the claim is not what authorizes it.
	if _, _, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", "org_1"); err != nil {
		t.Fatalf("buildRef: %v", err)
	}
	if !builds.started {
		t.Error("a grandfathered push stopped building; that would break every fleet on upgrade")
	}

	// And a claimed repository says nothing, or the warning is noise on every
	// push the fleet serves and nobody reads it.
	logged.Reset()
	if err := d.Store.PutRepoLink(context.Background(), &state.RepoLink{
		OrgID: "org_1", Repo: "gate/webjs-app", ConnectedAt: 1,
	}); err != nil {
		t.Fatalf("PutRepoLink: %v", err)
	}
	d.warnUnclaimed(context.Background(), svc, "org_1", "gate/webjs-app")
	if logged.Len() != 0 {
		t.Errorf("a claimed repository warned anyway: %s", logged.String())
	}
}

// A push unpacks and repacks a repository under the work root, not /tmp.
//
// /tmp on a systemd host is very commonly tmpfs, and this path holds a whole
// repository twice over. Doing that in the RAM of a host that is also running
// other tenants' microVMs is not something a push should be able to ask for.
//
// The staging directory is removed when buildRef returns, so it is observed
// from inside StartBuild, which runs while it is still there. Sampling it on a
// timer from another goroutine is a race that passes on a busy machine and
// fails on a fast one.
func TestAPushStagesUnderTheWorkRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "push-work")

	var staged []string
	builds := &recordingBuilds{onStart: func() {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			staged = append(staged, e.Name())
		}
	}}
	d := pushDeps(t, tarballOf(t, "webjs"), builds)
	d.WorkRoot = root

	if _, _, err := d.buildRef(context.Background(), pushEvent(), "abc1234", "shop", "org_1"); err != nil {
		t.Fatalf("buildRef: %v", err)
	}

	if len(staged) != 1 || !strings.HasPrefix(staged[0], "pilot-push-") {
		t.Errorf("staged %v under the work root, want one pilot-push-* directory; "+
			"the repository went to the process temp dir", staged)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("the work root still holds %v after the push", entries)
	}
}

// A delivery carries its own installation id. A caller that merely NAMES a
// repository does not, so Stage asks the App which installation covers it --
// without that, a browser-driven build could mint no token at all.
func TestStageResolvesTheInstallationWhenNoneIsGiven(t *testing.T) {
	fake := &fakeGitHub{tarball: tarballOf(t, "webjs")}
	d := pushDeps(t, nil, &recordingBuilds{})
	d.App.HTTP = &http.Client{Transport: fake}
	d.WorkRoot = t.TempDir()

	dir, err := d.Stage(context.Background(), 0, "gate/webjs-app", "abc1234")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer os.RemoveAll(dir)

	if len(fake.installations) != 1 || fake.installations[0] != "gate/webjs-app" {
		t.Errorf("looked up %v, want one lookup of gate/webjs-app", fake.installations)
	}
	// The wrapper directory GitHub adds is stripped on the way past, so the
	// repository's own files sit at the root the planner reads.
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		t.Errorf("the staged directory has no package.json at its root: %v", err)
	}
}

// A delivery's own installation id is used as given, and no lookup is made:
// the push path has one already and a second round trip per delivery would be
// a call GitHub rate-limits for nothing.
func TestStageUsesTheInstallationItIsGiven(t *testing.T) {
	fake := &fakeGitHub{tarball: tarballOf(t, "webjs")}
	d := pushDeps(t, nil, &recordingBuilds{})
	d.App.HTTP = &http.Client{Transport: fake}
	d.WorkRoot = t.TempDir()

	dir, err := d.Stage(context.Background(), 7, "gate/webjs-app", "abc1234")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer os.RemoveAll(dir)

	if len(fake.installations) != 0 {
		t.Errorf("looked up %v; the caller gave an installation", fake.installations)
	}
}
