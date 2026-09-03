package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/quota"
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
// maxBuildContext bounds an upload.
//
// A build runs an arbitrary user Dockerfile on a host that also runs other
// tenants' machines, so every input it takes needs a ceiling. 2 GiB is far
// past any reasonable source tree and far short of filling a host's disk.
const maxBuildContext = 2 << 30

func (d Deps) handleBuild(w http.ResponseWriter, r *http.Request) {
	if d.Builds == nil {
		writeJSON(w, http.StatusNotImplemented,
			ErrorResponse{Error: "builds are not configured on this host"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError,
			ErrorResponse{Error: "the server cannot stream"})
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
		writeJSON(w, http.StatusInternalServerError,
			ErrorResponse{Error: "cannot stage the build context: " + err.Error()})
		return
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	if _, err := io.Copy(spool, http.MaxBytesReader(w, r.Body, maxBuildContext)); err != nil {
		writeJSON(w, http.StatusBadRequest,
			ErrorResponse{Error: "reading the build context: " + err.Error()})
		return
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError,
			ErrorResponse{Error: "cannot rewind the build context: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", ndjson)
	// The id in a header as well as the stream: a client that wants to reattach
	// should not have to parse the body to learn what to reattach to.
	w.Header().Set("X-Pilot-Build-Id", id)
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	write := func(line BuildLogLine) {
		_ = enc.Encode(line)
		flusher.Flush()
	}
	write(BuildLogLine{
		Step: id, Stream: "status", Line: "build accepted",
		TS: time.Now().UnixMilli(),
	})

	// Deliberately NOT the request context. A build outlives the connection
	// that started it: a client that disconnects mid-build can reattach to the
	// log, and killing a ten-minute build because a laptop closed its lid is
	// not what anyone means by cancelling.
	buildID, err := d.Builds.StartBuild(context.WithoutCancel(r.Context()), id, spool, write)
	if err != nil {
		// The failing step was already emitted by the builder. This is the
		// terminal line, so that a consumer reading to the end always has a
		// verdict rather than having to infer one from the stream stopping.
		write(BuildLogLine{
			Step: id, Stream: "status", Line: "build failed",
			Error: err.Error(), TS: time.Now().UnixMilli(),
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
		writeJSON(w, http.StatusNotImplemented,
			ErrorResponse{Error: "builds are not configured on this host"})
		return
	}

	follow := r.URL.Query().Has("follow")
	backlog, live, found := d.Builds.BuildLog(r.Context(), r.PathValue("id"), follow)
	if !found {
		// Distinct from an empty log on purpose: a client that cannot tell
		// "this host does not have that build" from "that build printed
		// nothing" concludes the wrong thing about both.
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no such build on this host"})
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
