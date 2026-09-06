package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	StartBuild(ctx context.Context, id string, contextTar io.Reader,
		emit func(BuildLogLine)) (string, error)
	// BuildLog returns what was recorded and, when following, a channel of
	// what comes next. The bool reports whether this host has the build at all.
	BuildLog(ctx context.Context, id string, follow bool) ([]BuildLogLine, <-chan BuildLogLine, bool)
}

// ndjson is the media type of the build log stream: one JSON object per line,
// so a consumer can act on a failure the moment it appears rather than after
// the build finishes.
const ndjson = "application/x-ndjson"

// handleBuild accepts a context tar and streams the build.
//
// The response starts before the build does. That is what makes the stream
// useful -- a client watching a ten-minute build needs the first step's output
// in the first second -- and it has one consequence worth stating: the status
// code is decided before the outcome is known, so it is always 200 and the
// LAST line of the stream is what says whether the build worked. A line
// carrying `result` is a success; one carrying `error` is not.
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

	id := d.Builds.NewBuildID()

	// Concurrent builds are bounded per org ON THIS HOST. A build is not a
	// replicated object -- no row describes one -- so there is nothing
	// fleet-wide to count, and the refusal says "scope":"host" rather than
	// implying a limit this host cannot see.
	//
	// Taken before the context is spooled: refusing after accepting a 2 GiB
	// upload would make the limit cost more than the build it refused.
	org := OrgID(r.Context())
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
	defer d.BuildGate.Release(org)

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

	// Recorded BEFORE the id leaves this handler, in the header below and in
	// the stream. This is the JOB id, which scopes the log route. The image
	// the build produces has a second id, the rootfs build id, minted inside
	// the builder; that one is what a deploy and a create name, and its owner
	// is recorded in write below, on the first line that carries it, for the
	// same reason: an id that escapes before its owner is written is an id
	// anyone may boot.
	if err := d.Store.PutTenancy(r.Context(), &state.Tenancy{
		ID: id, OrgID: org, Kind: "build", CreatedAt: time.Now().Unix(),
	}); err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "cannot record the build's owner: "+err.Error(),
			NextInternal, nil)
		return
	}

	w.Header().Set("Content-Type", ndjson)
	// The id in a header as well as the stream: a client that wants to reattach
	// should not have to parse the body to learn what to reattach to.
	w.Header().Set("X-Pilot-Build-Id", id)
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
	write(BuildLogLine{
		Step: id, Stream: "status", Line: "build accepted",
		TS: time.Now().UnixMilli(),
	})

	buildID, err := d.Builds.StartBuild(bctx, id, spool, write)
	if err == nil && ownerErr != nil {
		err = fmt.Errorf("cannot record the image's owner: %w", ownerErr)
	}
	if err != nil {
		// The failing step was already emitted by the builder. This is the
		// terminal line, so that a consumer reading to the end always has a
		// verdict rather than having to infer one from the stream stopping.
		write(BuildLogLine{
			Step: id, Stream: "status", Line: "build failed",
			Error: err.Error(), Code: CodeBuildFailed, TS: time.Now().UnixMilli(),
		})
		return
	}
	write(BuildLogLine{
		Step: id, Stream: "status", Line: "build succeeded",
		Result: buildID, TS: time.Now().UnixMilli(),
	})
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
