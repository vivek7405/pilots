package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
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
}

// BuildRunner is the build surface, matching api.BuildRunner so the same
// builder serves both the public route and this one.
type BuildRunner interface {
	NewBuildID() string
	StartBuild(ctx context.Context, id string, contextTar io.Reader,
		emit func(api.BuildLogLine)) (string, error)
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

	build, err := d.buildRef(ctx, ev, ev.After)
	if err != nil {
		return err
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

	build, err := d.buildRef(ctx, ev, ev.PullRequest.Head.SHA)
	if err != nil {
		return err
	}

	// Replace rather than update: a preview is disposable and rebuilding it
	// from the new commit is simpler than reasoning about what changed.
	_ = d.destroyPreview(ctx, name, ev)

	// The preview belongs to the org that owns the service it previews. There
	// is no authenticated caller on a webhook delivery -- GitHub is the
	// principal -- so the org is read from the service rather than a request.
	previewOrg := ""
	if t, err := d.Store.GetTenancy(ctx, svc.ID); err == nil {
		previewOrg = t.OrgID
	}

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
	body := fmt.Sprintf("Preview for `%s`: https://%s\n\nIt suspends when idle and "+
		"wakes on the next request, and is destroyed when this pull request closes.",
		ev.PullRequest.Head.SHA[:min(7, len(ev.PullRequest.Head.SHA))], mach.Domain)
	return d.App.Comment(ctx, token, ev.Repository.FullName, ev.PullRequest.Number,
		previewMarker, body)
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

// buildRef fetches a ref's tarball and builds it.
func (d Deps) buildRef(ctx context.Context, ev Event, ref string) (string, error) {
	token, err := d.App.InstallationToken(ctx, ev.Installation.ID)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := d.App.Tarball(ctx, token, ev.Repository.FullName, ref, &buf); err != nil {
		return "", err
	}
	id := d.Builds.NewBuildID()
	// Logs are recorded by the builder and readable at
	// GET /v1/builds/{id}/logs, which is what a check run links to. Nothing is
	// emitted inline here: there is no client on the other end of a webhook.
	return d.Builds.StartBuild(ctx, id, &buf, func(api.BuildLogLine) {})
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
