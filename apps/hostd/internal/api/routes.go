package api

import (
	"encoding/json"
	"net/http"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Deps is what the handlers need from the rest of the process. It stays small
// on purpose: anything reachable only from one host does not belong here.
type Deps struct {
	HostID   string
	Store    state.Store
	Machines Manager
	// Reflink is the startup probe's result; see HealthResponse.Reflink.
	Reflink bool
}

// Routes registers the full public API. Phase 1 lands the shapes; the handlers
// answer 501 until Phase 2 implements them. Writing the table now means the
// CLI and SDKs can be built against a real route list, and a typo shows up as
// a failing test rather than a 404 in production.
func Routes(d Deps) http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: liveness and metrics.
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, HealthResponse{
			OK: true, HostID: d.HostID, Reflink: d.Reflink,
		})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(http.StatusOK)
	})

	// Machines: the one primitive.
	mux.HandleFunc("POST /v1/machines", d.handleCreateMachine)
	mux.HandleFunc("GET /v1/machines", d.handleListMachines)
	mux.HandleFunc("GET /v1/machines/{id}", d.handleGetMachine)
	mux.HandleFunc("DELETE /v1/machines/{id}", d.handleDestroyMachine)
	mux.HandleFunc("POST /v1/machines/{id}/exec", d.handleExec)
	mux.HandleFunc("GET /v1/machines/{id}/exec/stream", notImplemented)
	mux.HandleFunc("GET /v1/machines/{id}/logs", d.handleLogs)

	// Lifecycle. Suspend/wake are the scale-to-zero pair; stop/start are the
	// non-snapshotting equivalents.
	mux.HandleFunc("POST /v1/machines/{id}/suspend", d.handleSuspend)
	mux.HandleFunc("POST /v1/machines/{id}/wake", d.handleWake)
	mux.HandleFunc("POST /v1/machines/{id}/stop", notImplemented)
	mux.HandleFunc("POST /v1/machines/{id}/start", notImplemented)

	// Checkpoints. Restore is in place: same machine, same URL, same token.
	mux.HandleFunc("POST /v1/machines/{id}/checkpoints", d.handleCreateCheckpoint)
	mux.HandleFunc("GET /v1/machines/{id}/checkpoints", d.handleListCheckpoints)
	mux.HandleFunc("POST /v1/checkpoints/{id}/restore", d.handleRestoreCheckpoint)
	mux.HandleFunc("GET /v1/checkpoints/{id}", d.handleCheckpointStatus)

	// Builds: any Dockerfile to a bootable rootfs, with streamed NDJSON logs.
	mux.HandleFunc("POST /v1/builds", notImplemented)

	// Services and rollout.
	mux.HandleFunc("POST /v1/services", notImplemented)
	mux.HandleFunc("GET /v1/services", notImplemented)
	mux.HandleFunc("GET /v1/services/{id}", notImplemented)
	mux.HandleFunc("POST /v1/services/{id}/deploy", notImplemented)
	mux.HandleFunc("POST /v1/services/{id}/rollback", notImplemented)

	// Promote: the sandbox-to-production step, and the whole point of one
	// primitive serving both faces.
	mux.HandleFunc("POST /v1/machines/{id}/promote", notImplemented)

	// Volumes and fleet.
	mux.HandleFunc("POST /v1/volumes", notImplemented)
	mux.HandleFunc("GET /v1/volumes", notImplemented)
	mux.HandleFunc("GET /v1/hosts", d.handleListHosts)

	return WithAuth(d, mux)
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: "not implemented"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
