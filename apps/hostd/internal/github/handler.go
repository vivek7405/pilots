package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/build"
	"github.com/vivek7405/pilots/hostd/internal/compose"
	"github.com/vivek7405/pilots/hostd/internal/detect"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// previewMarker identifies our comment on a pull request so pushes update it
// rather than piling up.
const previewMarker = "<!-- pilots-preview -->"

// Deps is what turning a delivery into a deploy needs.
type Deps struct {
	HostID   string
	App      *App
	Store    state.Store
	Builds   BuildRunner
	Rollout  Rollout
	Machines MachineManager
	Domain   string
	// WorkRoot is where a push unpacks and repacks a repository. Empty falls
	// back to the process temp dir, which is what a test wants; a real host
	// passes the same cache root the builder stages under, so a large
	// repository never lands in tmpfs.
	WorkRoot string
	// URL renders a machine's hostname the way a client can open it, decided
	// once at startup exactly as it is for the API handlers (see
	// api.PublicURL). The zero value is the production shape -- https, no
	// port -- so a fleet that does not set it comments what it always did.
	URL api.PublicURL
}

// BuildRunner is the build surface, matching api.BuildRunner so the same
// builder serves both the public route and this one.
type BuildRunner interface {
	NewBuildID() string
	StartBuild(ctx context.Context, id string, contextTar io.Reader,
		emit func(api.BuildLogLine)) (string, error)
	// RecordRefusal writes a failed build log with no build run, so a push
	// the planner refused is readable at GET /v1/builds/{id}/logs exactly as
	// a failed build is.
	RecordRefusal(id string, line api.BuildLogLine)
}

type Rollout interface {
	Deploy(ctx context.Context, serviceID, rootfsBuildID string, knobs json.RawMessage) (*state.Release, error)
}

type MachineManager interface {
	Create(ctx context.Context, req api.CreateMachineRequest) (*state.Machine, error)
	Destroy(ctx context.Context, id string) error
}

// Handler is the webhook route, registered on every host.
func Handler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.App == nil {
			http.Error(w, "github app is not configured on this fleet", http.StatusServiceUnavailable)
			return
		}
		kind, ev, err := ReadDelivery(r, d.App.Secret)
		if err != nil {
			// 401, not 400: an unverified delivery is not a malformed request,
			// it is one this fleet cannot attribute to GitHub.
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		// Exactly one host acts. Every host receives deliveries because they
		// all sit behind the same wildcard DNS, so without this an N-host
		// fleet would run N builds for one push and race N deploys of the
		// same commit.
		if !d.mine(r.Context(), ev.Repository.FullName) {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Answer immediately and work in the background. GitHub times a
		// delivery out after ten seconds, and a build takes minutes -- a
		// synchronous handler would guarantee a redelivery of work already
		// running.
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
			defer cancel()
			if err := d.act(ctx, kind, ev); err != nil {
				slog.Error("could not act on a github delivery",
					"event", kind, "repo", ev.Repository.FullName, "err", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
	}
}

func (d Deps) mine(ctx context.Context, repo string) bool {
	hosts, err := d.Store.ListHosts(ctx)
	if err != nil {
		// Alone or unable to tell: act rather than drop the delivery. A
		// missed push is silent, and a duplicate build is merely wasteful.
		return true
	}
	live := make([]state.Host, 0, len(hosts))
	for _, h := range hosts {
		if time.Since(time.Unix(h.LastSeen, 0)) < 90*time.Second {
			live = append(live, h)
		}
	}
	owner, ok := state.OwnerFor(repo, live)
	return !ok || owner == d.HostID
}

func (d Deps) act(ctx context.Context, kind string, ev Event) error {
	switch kind {
	case "push":
		return d.onPush(ctx, ev)
	case "pull_request":
		return d.onPullRequest(ctx, ev)
	}
	return nil
}

// onPush deploys the tracked branch of a connected service.
func (d Deps) onPush(ctx context.Context, ev Event) error {
	svc, err := d.serviceFor(ctx, ev.Repository.FullName, ev.Branch())
	if err != nil || svc == nil {
		return err
	}
	if !svc.Autodeploy {
		return nil
	}

	build, step, err := d.buildRef(ctx, ev, ev.After, svc.App, d.orgOf(ctx, svc.ID))
	if err != nil {
		return err
	}
	// The health the plan worked out, when the service has none of its own.
	// Without this a repository with no Dockerfile deploys and then gates on
	// hostd's default check rather than on the readiness path its framework
	// actually serves, which is a rollout that passes before the app is up.
	//
	// Written on THIS host, the one the delivery elected. A row it may not
	// write comes back as state.ErrNotOwner, which is reported rather than
	// retried: retrying a write this host is not allowed to make cannot start
	// succeeding.
	if svc.Health == "" && step != nil && step.Health != nil {
		raw, err := json.Marshal(step.Health)
		if err != nil {
			return fmt.Errorf("github: encoding %s's health: %w", svc.ID, err)
		}
		svc.Health = string(raw)
		if err := d.Store.PutService(ctx, svc); err != nil {
			return fmt.Errorf("github: recording %s's health from the plan: %w", svc.ID, err)
		}
	}
	// A push carries no policy of its own; the replicas inherit whatever the
	// previous release's carry.
	_, err = d.Rollout.Deploy(ctx, svc.ID, build, nil)
	return err
}

// onPullRequest keeps a preview sandbox in step with a pull request.
//
// A SANDBOX, not a service. Giving a preview a service row would give it
// replicas, a health gate and a minimum running count -- which is to say a
// bill, for a branch that may be abandoned tomorrow. It is an idle-suspended
// machine that costs nothing between visits and is destroyed when the pull
// request closes.
func (d Deps) onPullRequest(ctx context.Context, ev Event) error {
	svc, err := d.serviceFor(ctx, ev.Repository.FullName, "")
	if err != nil || svc == nil {
		return err
	}
	name := fmt.Sprintf("pr-%d-%s", ev.PullRequest.Number, svc.Name)

	switch ev.Action {
	case "closed":
		return d.destroyPreview(ctx, name, ev)
	case "opened", "synchronize", "reopened":
	default:
		return nil
	}

	previewOrg := d.orgOf(ctx, svc.ID)

	build, _, err := d.buildRef(ctx, ev, ev.PullRequest.Head.SHA, svc.App, previewOrg)
	if err != nil {
		// A pull request's one surface is its comment, so a refusal says so
		// there. Otherwise the author sees a preview that never appeared and
		// no reason anywhere they can reach.
		if refusal := refusalOf(err); refusal != nil {
			if token, terr := d.App.InstallationToken(ctx, ev.Installation.ID); terr == nil {
				_ = d.App.Comment(ctx, token, ev.Repository.FullName, ev.PullRequest.Number,
					previewMarker, refusalComment(ev.PullRequest.Head.SHA, refusal))
			}
		}
		return err
	}

	// Replace rather than update: a preview is disposable and rebuilding it
	// from the new commit is simpler than reasoning about what changed.
	_ = d.destroyPreview(ctx, name, ev)

	mach, err := d.Machines.Create(ctx, api.CreateMachineRequest{
		Name:  name,
		Image: build,
		App:   svc.App,
		OrgID: previewOrg,
		// Suspends when idle and wakes on request, so an open pull request
		// nobody is looking at costs nothing.
		Knobs: []byte(`{"auto_stop":"suspend","auto_start":true,"min_machines_running":0}`),
	})
	if err != nil {
		return err
	}

	token, err := d.App.InstallationToken(ctx, ev.Installation.ID)
	if err != nil {
		return err
	}
	return d.App.Comment(ctx, token, ev.Repository.FullName, ev.PullRequest.Number,
		previewMarker, d.previewComment(ev.PullRequest.Head.SHA, mach.Domain))
}

// previewComment renders the comment a preview announces itself with.
//
// The URL goes through d.URL rather than a hardcoded https:// because this is
// the one place a client is handed a machine's address off the API path, and a
// link a developer cannot click is the whole defect: a single box serving the
// plain listener on :8080 rendered https://<name>.pilots.localhost here while
// every other surface rendered the port.
func (d Deps) previewComment(sha, domain string) string {
	return fmt.Sprintf("Preview for `%s`: %s\n\nIt suspends when idle and "+
		"wakes on the next request, and is destroyed when this pull request closes.",
		sha[:min(7, len(sha))], d.URL.Of(domain))
}

func (d Deps) destroyPreview(ctx context.Context, name string, ev Event) error {
	machines, err := d.Store.ListMachines(ctx)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if m.Name == name {
			return d.Machines.Destroy(ctx, m.ID)
		}
	}
	return nil
}

// Refusal is api.Refusal. An alias rather than a second type: the build route
// switches on it and lives in internal/api, which this package imports, so
// the type has to be declared there. Everything in this package, and every
// test of it, keeps saying Refusal.
type Refusal = api.Refusal

func refusalOf(err error) *Refusal {
	var r *Refusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// Stage fetches a ref's tarball and unpacks it, returning the directory. The
// CALLER removes it.
//
// Split out of buildRef so the push path, POST /v1/plan and POST /v1/builds
// share one fetch. A second implementation would be a second copy of the
// token, the tar reader and StripRoot, in a process that would then be holding
// repository bytes.
//
// installation 0 is resolved from the repository. A delivery knows its own
// installation; a caller that merely named a repository does not.
func (d Deps) Stage(ctx context.Context, installation int64, repo, ref string) (string, error) {
	if installation == 0 {
		id, err := d.App.InstallationFor(ctx, repo)
		if err != nil {
			return "", err
		}
		installation = id
	}
	token, err := d.App.InstallationToken(ctx, installation)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := d.App.Tarball(ctx, token, repo, ref, &buf); err != nil {
		return "", err
	}

	// Under the work root, not /tmp: a repository is unpacked here and then
	// repacked, and /tmp on a systemd host is very commonly tmpfs. Two copies
	// of a large repo in the RAM of a host running other tenants' microVMs is
	// not something a push should be able to ask for.
	if d.WorkRoot != "" {
		if err := os.MkdirAll(d.WorkRoot, 0o755); err != nil {
			return "", fmt.Errorf("github: staging %s: %w", d.WorkRoot, err)
		}
	}
	dir, err := os.MkdirTemp(d.WorkRoot, "pilot-push-*")
	if err != nil {
		return "", err
	}
	if err := build.ExtractContext(bytes.NewReader(buf.Bytes()), dir, api.MaxBuildContext); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("github: unpacking %s@%s: %w", repo, ref, err)
	}
	return dir, nil
}

// ContextOf plans a staged directory and returns the build context tar for the
// one step a plan may produce, plus that step.
//
// Planning first is what lets a repository with no Dockerfile deploy at all,
// and it is also where this has to stop: a plan with more than one service
// needs an order, a dependency graph and a shared app name, which is what a
// compose file is for. Executing one inside hostd would be a second executor
// beside the CLI's.
//
// The four refusals are recorded under id, so a person reads the reason at
// GET /v1/builds/{id}/logs, the same route a failed build's log is at. The
// caller owns dir and the returned file.
func (d Deps) ContextOf(ctx context.Context, id, dir, repo, app string) (*os.File, *compose.Step, error) {
	res, planErr, unknown, err := detect.Plan(ctx, dir, detect.Options{App: app})
	switch {
	case planErr != nil:
		return nil, nil, d.refuse(ctx, id, repo, api.CodePlanUnsupported, planErr.Error,
			"fix the listed keys in the compose file")
	case unknown != nil:
		return nil, nil, d.refuse(ctx, id, repo, api.CodeUnknownFramework, unknown.Error(),
			"commit a Dockerfile; pilot mcp can write one from the plan's details")
	case err != nil:
		return nil, nil, d.refuse(ctx, id, repo, api.CodeComposeInvalid, err.Error(),
			"fix the compose file")
	case len(res.Plan.Steps) != 1:
		return nil, nil, d.refuse(ctx, id, repo, api.CodePlanMultiService,
			fmt.Sprintf("the plan has %d services; a push deploys one", len(res.Plan.Steps)),
			"commit a compose file and deploy it with pilot deploy; a push deploys one service")
	}

	step := res.Plan.Steps[0]
	if step.Dockerfile != "" {
		// A recipe. Never over an existing file: the Dockerfile rung would
		// have won and this step would carry no text at all.
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"),
			[]byte(step.Dockerfile), 0o644); err != nil {
			return nil, nil, err
		}
	}
	slog.Info("planned a repository", "repo", repo, "build", id,
		"source", res.Detected[0].Source, "framework", res.Detected[0].Framework)

	tar, err := detect.TarDir(dir)
	if err != nil {
		return nil, nil, err
	}
	return tar, &step, nil
}

// buildRef stages a ref, plans it, and builds the one step a push may deploy.
//
// The build id is minted BEFORE the plan, so a refusal has a log a person can
// read at the same route a failed build's is at.
func (d Deps) buildRef(ctx context.Context, ev Event, ref, app, org string) (string, *compose.Step, error) {
	repo := ev.Repository.FullName
	dir, err := d.Stage(ctx, ev.Installation.ID, repo, ref)
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)

	id := d.Builds.NewBuildID()
	// The build's owner, recorded BEFORE anything is written under the id.
	// GET /v1/builds/{id}/logs is scoped by tenancy, so without this row the
	// refusal below -- and the log of a build that did run -- answers 404 to
	// every key but an admin one, which is to say it is readable by nobody the
	// push was for.
	if org != "" {
		if err := d.Store.PutTenancy(ctx, &state.Tenancy{
			ID: id, OrgID: org, Kind: "build", CreatedAt: time.Now().Unix(),
		}); err != nil {
			return "", nil, fmt.Errorf("github: recording build %s's owner: %w", id, err)
		}
	}

	tar, step, err := d.ContextOf(ctx, id, dir, repo, app)
	if err != nil {
		return "", nil, err
	}
	defer tar.Close()
	// Logs are recorded by the builder and readable at
	// GET /v1/builds/{id}/logs. Nothing is emitted inline here: there is no
	// client on the other end of a webhook.
	rootfs, err := d.Builds.StartBuild(ctx, id, tar, func(api.BuildLogLine) {})
	return rootfs, step, err
}

// orgOf is the org that owns a service, which is the org a push's build
// belongs to. There is no authenticated caller on a webhook delivery -- GitHub
// is the principal -- so ownership is read from the service rather than from a
// request. Empty when the service predates tenancy.
func (d Deps) orgOf(ctx context.Context, serviceID string) string {
	if t, err := d.Store.GetTenancy(ctx, serviceID); err == nil && t != nil {
		return t.OrgID
	}
	return ""
}

// refuse records a refusal where a person can read it and returns it as an
// error. There is no check-run integration, so the build log and the journal
// are the two places this can live, and it lives in both.
func (d Deps) refuse(ctx context.Context, id, repo string, code, msg, next string) error {
	d.Builds.RecordRefusal(id, api.BuildLogLine{
		Step: id, Stream: "status", Line: "refused",
		Error: msg, Code: code, TS: time.Now().UnixMilli(),
	})
	slog.Warn("refused a build from a repository", "repo", repo, "build", id,
		"code", code, "next", next)
	return &Refusal{BuildID: id, Code: code, Message: msg, Next: next}
}

// refusalComment is what a pull request whose preview was refused says.
func refusalComment(sha string, r *Refusal) string {
	return fmt.Sprintf("Preview for `%s` was not built: %s\n\nNext: %s",
		sha[:min(7, len(sha))], r.Message, r.Next)
}

// serviceFor finds the service connected to a repository, and to a branch when
// one is given.
func (d Deps) serviceFor(ctx context.Context, repo, branch string) (*state.Service, error) {
	svcs, err := d.Store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	for i := range svcs {
		if svcs[i].Repo != repo {
			continue
		}
		if branch != "" && svcs[i].Branch != "" && svcs[i].Branch != branch {
			continue
		}
		return &svcs[i], nil
	}
	return nil, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
