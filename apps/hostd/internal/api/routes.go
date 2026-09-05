package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/quota"
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
	// HugePages is this host's guest page size setting; see
	// HealthResponse.HugePages.
	HugePages bool
	// StoreVersion reads the replica's version, for HealthResponse.StoreVersion.
	// Nil on SQLite, where there is no replica and the field is 0.
	StoreVersion func(context.Context) (int64, error)
	// Builds turns a Dockerfile context into a rootfs build. Nil on a host
	// with no object storage, where a build has nowhere to publish to.
	Builds BuildRunner
	// Rollout deploys, rolls back, and promotes. Nil for the same reason
	// Builds is: a release has nowhere to come from without object storage.
	Rollout Rollout
	// Domain is the fleet's domain, for rendering a service's URL.
	Domain string
	// Resolver verifies that a custom hostname points here. Nil uses the
	// system resolver; a test supplies its own.
	Resolver Resolver
	// FleetKey seals secret environments before they are written. A service
	// create carrying secrets is refused without it rather than stored in the
	// clear -- those rows replicate to every host and into every backup.
	FleetKey Sealer
	// Peers resolves other hosts, so a service write that arrived at the
	// wrong host can be forwarded to the one allowed to perform it.
	Peers PeerLookup
	// PeerToken authenticates a call from another host of this fleet on the
	// internal listener. Derived from the agent-token secret every host
	// already shares, and accepted only on a request that carries the
	// forwarding marker, which the public listener strips. Empty on a single
	// box, where there are no peers to authenticate.
	PeerToken string
	// Compose plans a compose file. Injected as a handler because the compose
	// package imports this one for the wire structs its steps embed. Nil only
	// in tests, where the route answers 503 rather than vanishing from the
	// table -- a route that disappears in tests is a route nothing checks.
	Compose http.HandlerFunc
	// GitHub handles webhook deliveries. Nil when no app is configured, in
	// which case the route answers 503 rather than accepting deliveries it
	// cannot verify.
	GitHub http.HandlerFunc
	// Tenancy answers which org owns an object and whether a key has been
	// revoked, from local state. Nil falls back to the store, which is what a
	// single box wants; a fleet passes the subscription cache so neither
	// question costs a query on the request path.
	Tenancy TenancyView
	// BuildGate bounds concurrent builds per org on THIS host. Nil means
	// unlimited, which is what a test wants and what a host with no builder
	// configured never reaches.
	BuildGate *quota.HostGate
	// Lookup resolves a machine by NAME from an in-memory replica, sparing
	// the sprites alias a full ListMachines scan per request -- which on a
	// Corrosion host is a full-table query over HTTP to the local agent.
	// This is the same handle and the same cache the router's hot path
	// reads. Optional; nil, and a miss (a row the subscription has not
	// delivered yet), fall back to the store scan.
	Lookup func(name string) (state.Machine, bool)
	// LogFollowInterval and LogRowInterval are the two cadences a log follow
	// runs at: how often it reads the log file, and how often it re-reads the
	// row to notice a destroy. Zero means the defaults, which is what
	// production uses; a test shortens them so it need not wait out a real
	// destroy check.
	LogFollowInterval time.Duration
	LogRowInterval    time.Duration
	// Usage answers GET /v1/usage from this host's own ledger. Nil on a test
	// server, where the route answers an empty set of orgs rather than
	// vanishing from the table.
	Usage UsageSource
	// KeySource is where a minted key's randomness comes from. Nil is
	// crypto/rand, which is what production uses; a test supplies a fixed
	// reader so it can know the hash a mint will produce.
	KeySource io.Reader
}

// storeVersionTimeout bounds the one store read /v1/health does. Liveness has
// to answer on a host whose replica is wedged, not wait for it.
const storeVersionTimeout = 2 * time.Second

// Routes registers the full public API. Phase 1 lands the shapes; the handlers
// answer 501 until Phase 2 implements them. Writing the table now means the
// CLI and SDKs can be built against a real route list, and a typo shows up as
// a failing test rather than a 404 in production.
func Routes(d Deps) http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: liveness and metrics.
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		resp := HealthResponse{
			OK: true, HostID: d.HostID, Reflink: d.Reflink,
			HugePages: d.HugePages,
		}
		if d.StoreVersion != nil {
			// Bounded, and short. The corrosion client sets no response
			// timeout, so an agent that accepts the connection and then stops
			// answering would hold this handler open until the client gave
			// up -- on the one unauthenticated route every load balancer
			// polls, which is the opposite of what the paragraph below
			// promises. A hung replica has to read as "version 0", fast.
			ctx, cancel := context.WithTimeout(r.Context(), storeVersionTimeout)
			defer cancel()
			// A replica that cannot be read is not a dead host: liveness
			// still answers 200 and the version stays 0, because a health
			// check that fails on a store hiccup takes the host out of
			// rotation for a problem that is not the host's.
			if v, err := d.StoreVersion(ctx); err != nil {
				slog.Warn("could not read the store version", "err", err)
			} else {
				resp.StoreVersion = v
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		// Collected on the scrape rather than on a timer: the memory handlers
		// are separate processes that have to be asked, and asking them on a
		// schedule nobody is reading is work for nothing.
		if d.Machines != nil {
			d.Machines.CollectMetrics()
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(http.StatusOK)
		metrics.Default.Render(w)
	})

	// Machines: the one primitive.
	mux.HandleFunc("POST /v1/machines", d.handleCreateMachine)
	mux.HandleFunc("GET /v1/machines", d.handleListMachines)
	mux.HandleFunc("GET /v1/machines/{id}", d.handleGetMachine)
	mux.HandleFunc("DELETE /v1/machines/{id}", d.handleDestroyMachine)
	mux.HandleFunc("POST /v1/machines/{id}/exec", d.handleExec)
	mux.HandleFunc("GET /v1/machines/{id}/exec/stream", d.handleExecStream)
	// The sprites alias, name-keyed: see handleSpriteExec.
	mux.HandleFunc("GET /v1/sprites/{name}/exec", d.handleSpriteExec)
	mux.HandleFunc("GET /v1/machines/{id}/logs", d.handleLogs)

	// Lifecycle. Suspend/wake are the scale-to-zero pair; stop/start are the
	// non-snapshotting equivalents.
	mux.HandleFunc("POST /v1/machines/{id}/suspend", d.handleSuspend)
	mux.HandleFunc("POST /v1/machines/{id}/wake", d.handleWake)
	// Redeploy is the rollout's: the same machine, booted from another image.
	mux.HandleFunc("POST /v1/machines/{id}/redeploy", d.handleRedeploy)
	mux.HandleFunc("POST /v1/machines/{id}/stop", notImplemented)
	mux.HandleFunc("POST /v1/machines/{id}/start", notImplemented)

	// Checkpoints. Restore is in place: same machine, same URL, same token.
	mux.HandleFunc("POST /v1/machines/{id}/checkpoints", d.handleCreateCheckpoint)
	mux.HandleFunc("GET /v1/machines/{id}/checkpoints", d.handleListCheckpoints)
	mux.HandleFunc("POST /v1/checkpoints/{id}/restore", d.handleRestoreCheckpoint)
	mux.HandleFunc("GET /v1/checkpoints/{id}", d.handleCheckpointStatus)

	// Builds: any Dockerfile to a bootable rootfs, with streamed NDJSON logs.
	mux.HandleFunc("POST /v1/builds", d.handleBuild)
	mux.HandleFunc("GET /v1/builds/{id}/logs", d.handleBuildLogs)

	// Services and rollout.
	mux.HandleFunc("POST /v1/services", d.handleCreateService)
	mux.HandleFunc("GET /v1/services", d.handleListServices)
	mux.HandleFunc("GET /v1/services/{id}", d.handleGetService)
	mux.HandleFunc("PATCH /v1/services/{id}", d.handleUpdateService)
	mux.HandleFunc("GET /v1/services/{id}/releases", d.handleListReleases)
	mux.HandleFunc("POST /v1/services/{id}/deploy", d.handleDeploy)
	mux.HandleFunc("POST /v1/services/{id}/rollback", d.handleRollback)

	// Promote: the sandbox-to-production step, and the whole point of one
	// primitive serving both faces.
	mux.HandleFunc("POST /v1/machines/{id}/promote", d.handlePromote)

	// The compose plan. Injected because internal/compose imports this package
	// for the wire types its steps embed, so the import cannot go both ways.
	if d.Compose != nil {
		mux.HandleFunc("POST /v1/compose/plan", d.Compose)
	} else {
		mux.HandleFunc("POST /v1/compose/plan", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable,
				ErrorResponse{Error: "no compose planner on this host"})
		})
	}

	// Custom domains. Verification is what stops a caller spending the
	// fleet's shared certificate rate limit on a name they do not own.
	// The GitHub webhook. Unauthenticated by API key on purpose: it carries
	// its own HMAC signature, which is the only credential GitHub can present.
	if d.GitHub != nil {
		mux.HandleFunc("POST /v1/github/webhook", d.GitHub)
	} else {
		mux.HandleFunc("POST /v1/github/webhook", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable,
				ErrorResponse{Error: "no github app is configured on this fleet"})
		})
	}

	mux.HandleFunc("POST /v1/domains", d.handleAddDomain)
	mux.HandleFunc("GET /v1/domains", d.handleListDomains)
	mux.HandleFunc("DELETE /v1/domains/{hostname}", d.handleDeleteDomain)

	// Volumes and fleet.
	mux.HandleFunc("POST /v1/volumes", d.handleCreateVolume)
	mux.HandleFunc("GET /v1/volumes", d.handleListVolumes)
	// The volume drive as Firecracker holds it, not as hostd meant to set it.
	// See MachineVolume: the difference between the two is a durability
	// guarantee that fails silently.
	mux.HandleFunc("GET /v1/machines/{id}/volume", d.handleMachineVolume)
	mux.HandleFunc("GET /v1/hosts", d.handleListHosts)

	// Tenancy administration. Admin-scoped, and served by every host from its
	// own replica: the dashboard is a guest on the platform and cannot reach
	// its host directly, so minting a key is an ordinary public API call.
	mux.HandleFunc("POST /v1/api-keys", d.handleCreateAPIKey)
	mux.HandleFunc("GET /v1/api-keys", d.handleListAPIKeys)
	mux.HandleFunc("POST /v1/api-keys/{hash}/revoke", d.handleRevokeAPIKey)
	mux.HandleFunc("GET /v1/usage", d.handleUsage)
	mux.HandleFunc("GET /v1/quotas/{org}", d.handleGetQuota)
	mux.HandleFunc("PUT /v1/quotas/{org}", d.handlePutQuota)

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

// Sealer seals a secret environment. Satisfied by seal.Key.
type Sealer interface {
	IsSet() bool
	Seal([]byte) (string, error)
}
