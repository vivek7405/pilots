package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// BuildRunner is the build surface the handlers drive.
//
// Expressed in this package's own types so the dependency runs one way: the
// builder knows about the log-line contract, and the API layer knows nothing
// about BuildKit, mke2fs or the chunk store.
type BuildRunner interface {
	// NewBuildID mints the id the caller will follow logs by. Handed out
	// before the build starts, because a client that loses the streaming
	// connection has to be able to reattach -- and it cannot reattach to an id
	// it was going to be told at the end.
	NewBuildID() string
	// StartBuild runs a build to completion, calling emit for every line as it
	// happens, and returns the rootfs build id.
	// orgID names the tenant whose builder machine the solve runs in. A
	// build executes inside that org's own guest, so it is not optional
	// context: an empty org is the platform's own build, never a tenant's.
	StartBuild(ctx context.Context, id, orgID string, contextTar io.Reader,
		emit func(BuildLogLine)) (string, error)
	// BuildLog returns what was recorded and, when following, a channel of
	// what comes next. The bool reports whether this host has the build at all.
	BuildLog(ctx context.Context, id string, follow bool) ([]BuildLogLine, <-chan BuildLogLine, bool)
	// RecordRefusal writes a failed build log with no build run, so a push
	// the planner refused reads back at GET /v1/builds/{id}/logs the way a
	// failed build does. On this interface rather than only on the GitHub
	// one, because the route that serves those logs is here.
	RecordRefusal(id string, line BuildLogLine)
}

// BuildLogHolder is the extra a build that ends in a RELEASE needs: its log
// has to outlive the build itself.
//
// A browser watching a build reads GET /v1/builds/{id}/logs, not the response
// to the POST that started it -- the tab that posted may never have existed,
// and the connection that did is closed the moment the build is under way. So
// the rollout's verdict, minutes after the image is published, has to be a
// line in the recorded log; without a hold the builder closes that log when
// the image exists and every follower is released before the interesting part.
//
// Separate from BuildRunner, and taken with a type assertion, so a runner that
// only builds stays a runner that only builds. internal/build asserts that the
// real builder satisfies it at compile time.
type BuildLogHolder interface {
	// HoldLog keeps a build's log open past the end of the build. Called
	// BEFORE the build, which is what creates the log.
	HoldLog(id string)
	// RecordLine appends a line to the recorded log, for every follower.
	RecordLine(id string, line BuildLogLine)
	// ReleaseLog ends the hold and lets the followers go. Exactly one call
	// per HoldLog, on every path out.
	ReleaseLog(id string)
}

// ndjson is the media type of the build log stream: one JSON object per line,
// so a consumer can act on a failure the moment it appears rather than after
// the build finishes.
const ndjson = "application/x-ndjson"

// handleBuild accepts a context tar, or a repository to fetch one from, and
// streams the build.
//
// The response starts before the build does. That is what makes the stream
// useful -- a client watching a ten-minute build needs the first step's output
// in the first second -- and it has one consequence worth stating: the status
// code is decided before the outcome is known, so it is always 200 and the
// LAST line of the stream is what says whether the build worked. A line
// carrying `result` is a success; one carrying `error` is not.
//
// A JSON body is the exception: it names a repository, so the fetch and the
// plan both happen BEFORE any of that, and their failures are ordinary status
// codes. Nothing has been streamed yet when they are decided.
//
// `?deploy=<service>` carries the DEPLOY INTENT with the build: the host that
// built the image cuts the release itself, on the verdict, exactly once. The
// alternative -- a client that watches the stream and posts the deploy when it
// sees the image id -- makes the release only as reliable as whoever is
// watching: a closed tab is a successful build that deployed nothing, and two
// tabs are two rollouts of one image.
// MaxBuildContext bounds an upload. Exported because internal/detect serves
// POST /v1/plan under the same ceiling, from the same tar.
//
// A build runs an arbitrary user Dockerfile on a host that also runs other
// tenants' machines, so every input it takes needs a ceiling. 2 GiB is far
// past any reasonable source tree and far short of filling a host's disk.
const MaxBuildContext = 2 << 30

func (d Deps) handleBuild(w http.ResponseWriter, r *http.Request) {
	if d.Builds == nil {
		WriteError(w, http.StatusNotImplemented, CodeNotConfigured,
			"builds are not configured on this host",
			"deploy from a host that runs BuildKit; pilot status lists hosts", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "the server cannot stream", NextInternal, nil)
		return
	}

	// The deploy this build is for, if it is for one. Everything it needs is
	// decided HERE, before a byte is streamed and before a ten-minute build:
	// a refusal a person can act on is worth nothing ten minutes late.
	deployTo := r.URL.Query().Get("deploy")
	var holder BuildLogHolder
	if deployTo != "" {
		// Ownership before forwarding, exactly as handleDeploy does it: a
		// foreign service must not be told which host arbitrates it.
		if _, ok := d.ownedService(w, r, deployTo); !ok {
			return
		}
		// Only the arbiter may write a service, so the BUILD goes to the
		// arbiter too. The release is cut in this handler once the image
		// exists, and a rollout attempted anywhere else is refused by the
		// store's single-writer check -- minutes after the point where the
		// person could have been told, with an image built and nothing
		// deployed. One hop, the same proxy every service write takes.
		if d.forwardToArbiter(w, r, deployTo) {
			return
		}
		if d.Rollout == nil {
			WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
				"this host cannot deploy: no object storage is configured",
				"deploy from a host with object storage; pilot status lists hosts", nil)
			return
		}
		holder, _ = d.Builds.(BuildLogHolder)
		if holder == nil {
			// Refused rather than degraded. The deploy would still happen,
			// but its verdict would exist only on the connection that started
			// the build -- which is the failure this whole route shape exists
			// to remove.
			WriteError(w, http.StatusNotImplemented, CodeNotImplemented,
				"this host's builder cannot follow a build through to a release",
				"build without deploy=, then POST /v1/services/{id}/deploy with the image id", nil)
			return
		}
		// A rollout boots one extra machine before it retires the old one, so
		// the deploy is admitted against one replica's worth of headroom --
		// here, rather than after the build has already run.
		if !d.checkQuota(w, r, quota.Delta{Machines: 1, VCPUs: 1, MemMiB: 512}) {
			return
		}
	}

	id := d.Builds.NewBuildID()

	if holder != nil {
		// The log outlives the build, because the release is cut after the
		// image exists and its verdict belongs in the same log. Released on
		// EVERY path out of this handler: a held log nobody releases never
		// lets its followers go.
		holder.HoldLog(id)
		defer holder.ReleaseLog(id)
	}

	// Concurrent builds are bounded per org ON THIS HOST. A build is not a
	// replicated object -- no row describes one -- so there is nothing
	// fleet-wide to count, and the refusal says "scope":"host" rather than
	// implying a limit this host cannot see.
	//
	// Taken before the context is spooled: refusing after accepting a 2 GiB
	// upload would make the limit cost more than the build it refused.
	// The repository body is read and refused BEFORE anything is recorded: the
	// tenancy row below gossips fleet-wide, and a bad body, a fleet with no
	// App or a key that may not name a repository must not each leave one.
	var ref RepoRef
	named := isJSONBody(r)
	if named {
		var ok bool
		if ref, ok = d.parseRepoRef(w, r); !ok {
			return
		}
	}

	org := actingOrg(r)
	limits := quota.For(r.Context(), d.Store, org)
	if used, ok := d.BuildGate.Acquire(org, limits.MaxBuilds); !ok {
		// The build gate is the one refusal that does not go through
		// writeQuotaError, so it counts itself.
		metrics.QuotaRefusals.With("builds").Inc()
		writeJSON(w, http.StatusTooManyRequests, QuotaExceededResponse{
			Error: "quota exceeded", Quota: "builds",
			Limit: limits.MaxBuilds, Used: used, Scope: "host",
		})
		return
	}
	// Released as soon as the BUILD is over, not when the handler is. A build
	// that carries a deploy stays in this handler for the rollout as well --
	// a health grace of minutes -- and a slot held across that refuses the
	// org's next build with "quota exceeded, builds" while nothing is
	// building. The defer is the early-exit paths; the explicit call below is
	// the one that matters, and OnceFunc makes the pair safe.
	releaseGate := sync.OnceFunc(func() { d.BuildGate.Release(org) })
	defer releaseGate()

	// The build's owner, recorded before ANY branch below can hand the id out.
	// GET /v1/builds/{id}/logs is scoped by tenancy, so a refusal recorded
	// under an id with no owner row is readable by nobody it was for. Moved
	// above the two branches rather than duplicated into each: the repository
	// branch records a refusal, and the refusal is the whole answer.
	//
	// This is the JOB id, which scopes the log route. The image the build
	// produces has a second id, the rootfs build id, minted inside the
	// builder; that one is what a deploy and a create name, and its owner is
	// recorded in write below, on the first line that carries it, for the same
	// reason: an id that escapes before its owner is written is an id anyone
	// may boot.
	if err := d.Store.PutTenancy(r.Context(), &state.Tenancy{
		ID: id, OrgID: org, Kind: "build", CreatedAt: time.Now().Unix(),
	}); err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "cannot record the build's owner: "+err.Error(),
			NextInternal, nil)
		return
	}

	// The id goes out BEFORE the repository branch: a refusal below is a 400
	// with no id in the body, and the log it was recorded under is only
	// reachable through this header. The tenancy row is already written, so
	// the id is safe to hand out.
	w.Header().Set("X-Pilot-Build-Id", id)

	// A repository named rather than sent. The host fetches and plans it
	// through the fleet's GitHub App, the path a push takes, so a client that
	// holds no repository bytes can still build.
	var contextTar io.ReadCloser
	if named {
		rc, ok := d.stageRepo(w, r, id, ref)
		if !ok {
			return
		}
		defer rc.Close()
		contextTar = rc
	}

	// Spool the context to disk BEFORE writing a single byte of response.
	//
	// Not an optimisation -- a correctness fix. Go's server treats the request
	// as finished once the handler starts writing a streamed response, so the
	// first read of r.Body after that returns "http: invalid Read on closed
	// Body" and every build fails at its first instruction. The upload has to
	// be fully consumed while the request is still a request.
	//
	// A file rather than memory: a context is arbitrary user data, and holding
	// it in RAM on a host that also runs other tenants' machines makes a large
	// upload a memory-exhaustion lever.
	if contextTar == nil {
		spool, err := os.CreateTemp("", "pilot-build-context-*.tar")
		if err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal, "cannot stage the build context: "+err.Error(),
				NextInternal, nil)
			return
		}
		defer func() {
			spool.Close()
			os.Remove(spool.Name())
		}()

		if _, err := io.Copy(spool, http.MaxBytesReader(w, r.Body, MaxBuildContext)); err != nil {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "reading the build context: "+err.Error(),
				"the context is over 2 GiB or the upload was cut; add a .dockerignore", nil)
			return
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal, "cannot rewind the build context: "+err.Error(),
				NextInternal, nil)
			return
		}
		contextTar = spool
	}

	w.Header().Set("Content-Type", ndjson)
	// The id is already in the header (set above, before the repository
	// branch) as well as in the stream: a client that wants to reattach should
	// not have to parse the body to learn what to reattach to.
	w.WriteHeader(http.StatusOK)

	// Deliberately NOT the request context, for the build and for the owner
	// row alike. A build outlives the connection that started it: a client
	// that disconnects mid-build can reattach to the log, and killing a
	// ten-minute build because a laptop closed its lid is not what anyone
	// means by cancelling. The row is written minutes in, and a client that
	// has gone must not turn a finished image into one nobody owns.
	bctx := context.WithoutCancel(r.Context())

	enc := json.NewEncoder(w)
	// ownerErr is set if the rootfs build id's owner could not be recorded.
	// The build is then reported as failed, and the image is orphaned in
	// object storage exactly as a failed upload's is.
	//
	// Scoped precisely, because the guarantee is narrower than it first
	// reads: this rewrites the STREAM. The builder appends every line to its
	// own log store BEFORE it emits, so the recorded copy still ends on the
	// builder's "build complete" line carrying the id, and a later GET on the
	// log replays that with no failure line in it. What is guaranteed is that
	// the id is never handed out USABLE, not that no copy of it is readable.
	// It holds because the log route is scoped by the job's own tenancy row
	// to the org that built it, so the id reaches nobody who was not going to
	// own it, and with no owner row every deploy, create and redeploy refuses
	// it. Unusable rather than ownerless.
	var ownerErr error
	ownerDone := false
	write := func(line BuildLogLine) {
		if line.Result != "" {
			// The rootfs build id is first seen here, on the builder's own
			// "build complete" line, and again on the terminal line below.
			// Written before either is encoded, and ONCE: the store is
			// write-once, but the call is a round trip that can fail on its
			// own, and a second one failing where the first succeeded would
			// report a finished, owned image as a failed build.
			if !ownerDone {
				ownerErr = d.Store.PutTenancy(bctx, &state.Tenancy{
					ID: line.Result, OrgID: org, Kind: "build", CreatedAt: time.Now().Unix(),
				})
				ownerDone = ownerErr == nil
			}
			if ownerErr != nil {
				line = BuildLogLine{
					Step: id, Stream: "status", Line: "build failed",
					Error: "cannot record the image's owner: " + ownerErr.Error(),
					Code:  CodeBuildFailed,
					TS:    time.Now().UnixMilli(),
				}
			}
		}
		_ = enc.Encode(line)
		flusher.Flush()
	}
	// say is write for the lines that come AFTER the builder's own. The
	// builder records every line it emits, so a follower of the log route
	// sees the build; these are written once it has returned, and a deploying
	// build has to record them too or the person watching the log sees the
	// image and never the release it became.
	say := func(line BuildLogLine) {
		if holder != nil {
			holder.RecordLine(id, line)
		}
		write(line)
	}

	write(BuildLogLine{
		Step: id, Stream: "status", Line: "build accepted",
		TS: time.Now().UnixMilli(),
	})

	buildID, err := d.Builds.StartBuild(bctx, id, org, contextTar, write)
	// The build is over either way; what follows is a rollout, which is not a
	// build and must not hold a build's slot.
	releaseGate()
	if err == nil && ownerErr != nil {
		err = fmt.Errorf("cannot record the image's owner: %w", ownerErr)
	}
	if err != nil {
		// The failing step was already emitted by the builder. This is the
		// terminal line, so that a consumer reading to the end always has a
		// verdict rather than having to infer one from the stream stopping.
		say(BuildLogLine{
			Step: id, Stream: "status", Line: "build failed",
			Error: err.Error(), Code: CodeBuildFailed, TS: time.Now().UnixMilli(),
		})
		return
	}
	if deployTo == "" {
		say(BuildLogLine{
			Step: id, Stream: "status", Line: "build succeeded",
			Result: buildID, TS: time.Now().UnixMilli(),
		})
		return
	}

	say(BuildLogLine{
		Step: id, Stream: "status", Line: "build succeeded, deploying " + deployTo,
		Result: buildID, TS: time.Now().UnixMilli(),
	})
	// The release, cut here, by the host that built the image.
	//
	// bctx and not the request's context, for the reason the build itself
	// runs on bctx: this is the half that must not depend on anyone watching.
	// A rollout gates a replica for as long as the health check's grace
	// period, which is routinely minutes, and a person who closed the tab --
	// or a laptop that closed its lid -- must not be the reason a successful
	// build deployed nothing.
	rel, derr := d.Rollout.Deploy(bctx, deployTo, buildID, nil)
	if derr != nil {
		// The engine's own words, on the line a follower reads as the
		// verdict: the same message, code and next a POST to the deploy route
		// would have answered with. A health gate that never passed names the
		// replica to read the console of, and that is the whole value of it.
		_, body := mapError(derr)
		say(BuildLogLine{
			Step: id, Stream: "status", Line: "deploy refused",
			Error: body.Error, Code: body.Code, Next: body.Next,
			TS: time.Now().UnixMilli(),
		})
		return
	}
	say(BuildLogLine{
		Step: id, Stream: "status", Line: "deployed " + rel.ID,
		Result: buildID, Release: rel.ID, TS: time.Now().UnixMilli(),
	})
}

// isJSONBody reports whether the body names a repository rather than carrying
// a tar. The media type, not a sniff of the bytes: a tar whose first bytes
// happen to look like JSON must not change which branch runs.
func isJSONBody(r *http.Request) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && ct == "application/json"
}

// stageRepo turns a RepoRef body into a build context, writing the refusal
// itself when it cannot. Nothing has been streamed when it answers, so every
// failure here is an ordinary status code rather than a line in a 200.
// RepoSlug is the only shape a repository may take on the wire: `owner/name`,
// each half a GitHub name, nothing else.
//
// The API is the trust boundary, not the dashboard. `ref.Repo` is interpolated
// into `/repos/%s/installation` under the App JWT and into the tarball URL
// under an installation token, so a `..`, a `?` or a `#` in it rewrites the
// path or the query of a request made with the fleet's own credential.
var RepoSlug = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}/[A-Za-z0-9._-]{1,100}$`)

// parseRepoRef reads and validates the {repo, ref} body, and reports whether
// this caller may name a repository at all.
//
// Split from the fetch so every one of these refusals lands BEFORE the build's
// tenancy row is written: a row gossips to every host on the fleet, and a
// client retrying a bad body on a fleet with no App would otherwise write one
// per attempt. The planner's refusal still comes after, because that verdict
// is recorded under the build id and has to be readable at its log.
func (d Deps) parseRepoRef(w http.ResponseWriter, r *http.Request) (RepoRef, bool) {
	var ref RepoRef
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ref); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"reading the repository: "+err.Error(), NextBadBody, nil)
		return ref, false
	}
	if ref.Repo == "" || ref.Ref == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"repo and ref are both required",
			`send {"repo":"owner/name","ref":"main"}`, nil)
		return ref, false
	}
	if !RepoSlug.MatchString(ref.Repo) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"repo must be owner/name",
			`send {"repo":"owner/name","ref":"main"}`, nil)
		return ref, false
	}
	// Whether this ORG may have the fleet fetch this repository, answered from
	// local state (AllowRepo, repos.go). It replaces the admin-only gate that
	// stood here: the App's installation token can fetch every repository the
	// fleet's App is installed on, so something has to tie the caller to the
	// one it named, and now something does.
	//
	// BEFORE the no-App check below, and deliberately: a caller with no claim
	// on a repository is refused whether or not this fleet could have fetched
	// it, and the refusal then says the same thing on every fleet rather than
	// depending on how the host is configured.
	if !AllowRepo(w, r, d.Store, ref.Repo) {
		return ref, false
	}
	if d.Repos == nil {
		// 503 and not 501: the route exists and works on a fleet whose hosts
		// carry an App. Naming the tar is what makes this actionable without
		// an operator, since every client that can build can send one.
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this fleet has no GitHub App, so it cannot fetch a repository",
			"send a tar of the directory, or set PILOT_GITHUB_APP_ID and PILOT_GITHUB_APP_KEY on every host", nil)
		return ref, false
	}
	return ref, true
}

// stageRepo fetches a validated ref, and records the planner's verdict under
// the build id when it refuses.
func (d Deps) stageRepo(w http.ResponseWriter, r *http.Request, id string, ref RepoRef) (io.ReadCloser, bool) {

	// Not the request context. The fetch and the plan are recorded under the
	// build id, and a client that hung up must not leave a half-written
	// refusal nobody can read.
	rc, err := d.Repos.Context(context.WithoutCancel(r.Context()), id,
		ref.Repo, ref.Ref, r.URL.Query().Get("app"))
	if err == nil {
		return rc, true
	}

	// A refusal is the planner's verdict on the repository, so it is a 400
	// with the planner's own code. The codes are constants because
	// errors_test.go walks every WriteError call and refuses one it cannot
	// see, which a refusal.Code passed straight through would be.
	var refusal *Refusal
	if errors.As(err, &refusal) {
		next := refusal.Next
		switch refusal.Code {
		case CodePlanUnsupported:
			WriteError(w, http.StatusBadRequest, CodePlanUnsupported, refusal.Message, next, nil)
		case CodeUnknownFramework:
			WriteError(w, http.StatusBadRequest, CodeUnknownFramework, refusal.Message, next, nil)
		case CodeComposeInvalid:
			WriteError(w, http.StatusBadRequest, CodeComposeInvalid, refusal.Message, next, nil)
		case CodePlanMultiService:
			WriteError(w, http.StatusBadRequest, CodePlanMultiService, refusal.Message, next, nil)
		default:
			WriteError(w, http.StatusBadRequest, CodeBadRequest, refusal.Message, next, nil)
		}
		return nil, false
	}
	// 502 and not 500: the failure is GitHub's answer, not this host's state,
	// and the caller's next step is to check the App's access to that
	// repository rather than to retry.
	WriteError(w, http.StatusBadGateway, CodeUnavailable,
		"fetching "+ref.Repo+"@"+ref.Ref+": "+err.Error(),
		"check that the fleet's GitHub App is installed on that repository and the ref exists", nil)
	return nil, false
}

// handleBuildLogs replays a build's log, optionally following it live.
//
// The same NDJSON lines the build itself streamed, so a client that lost its
// connection reattaches to an identical stream rather than a second format.
func (d Deps) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	if d.Builds == nil {
		WriteError(w, http.StatusNotImplemented, CodeNotConfigured,
			"builds are not configured on this host",
			"deploy from a host that runs BuildKit; pilot status lists hosts", nil)
		return
	}

	// A build log is the build's own output: Dockerfile lines, registry URLs,
	// whatever the build echoed. It is scoped like the build it belongs to.
	if !d.ownedBuild(w, r, r.PathValue("id")) {
		return
	}

	follow := r.URL.Query().Has("follow")
	backlog, live, found := d.Builds.BuildLog(r.Context(), r.PathValue("id"), follow)
	if !found {
		// Distinct from an empty log on purpose: a client that cannot tell
		// "this host does not have that build" from "that build printed
		// nothing" concludes the wrong thing about both.
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such build on this host",
			NextNotFound, nil)
		return
	}

	w.Header().Set("Content-Type", ndjson)
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	for _, line := range backlog {
		if enc.Encode(line) != nil {
			return
		}
	}
	flush()

	if live == nil {
		return
	}
	for {
		select {
		case line, open := <-live:
			if !open {
				return
			}
			if enc.Encode(line) != nil {
				return
			}
			flush()
		case <-r.Context().Done():
			return
		}
	}
}
