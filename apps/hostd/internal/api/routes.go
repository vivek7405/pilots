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
	// Replication reads how far this replica has caught up, for the three
	// replication fields of HealthResponse: the version vector's sum, the
	// vector itself, and whether the join gate has opened. Nil on SQLite,
	// where there is no replica, the versions are empty and complete is true.
	Replication func(context.Context) (int64, map[string]int64, bool, error)
	// Builds turns a Dockerfile context into a rootfs build. Nil on a host
	// with no object storage, where a build has nowhere to publish to.
	Builds BuildRunner
	// Rollout deploys, rolls back, and promotes. Nil for the same reason
	// Builds is: a release has nowhere to come from without object storage.
	Rollout Rollout
	// Domain is the fleet's domain, for rendering a service's URL.
	Domain string
	// APIHostname is the control API's own hostname, the one label a tenant
	// may not take. A service holding it would own a URL it could never be
	// reached at, because dispatch claims that hostname before the workload
	// suffix check. Empty means "api." + Domain, the same default
	// machines.Options takes.
	APIHostname string
	// URL is the scheme and port every machine and service URL is rendered
	// with. See PublicURL; the zero value is the production shape.
	URL PublicURL
	// Resolver verifies that a custom hostname points here. Nil uses the
	// system resolver; a test supplies its own.
	Resolver Resolver
	// SelfURL is where this process reaches its own plain listener,
	// http://127.0.0.1:<port>. The hosted MCP endpoint builds an SDK client
	// against it per request, carrying the caller's key, so every tool call
	// takes the public API's own path: auth, scopes, tenancy, forwarding.
	SelfURL string
	// DashboardURL is the fleet's dashboard origin, named as the OAuth
	// authorization server in the protected-resource document an MCP client
	// reads after a 401. Empty on a fleet without one; the document then
	// names no server and the client is expected to carry a key in a header.
	DashboardURL string
	// FleetKey seals secret environments before they are written. A service
	// create carrying secrets is refused without it rather than stored in the
	// clear -- those rows replicate to every host and into every backup.
	FleetKey Sealer
	// Peers resolves other hosts, so a service write that arrived at the
	// wrong host can be forwarded to the one allowed to perform it.
	Peers PeerLookup
	// Placement counts where creates ended up, so an operator can see whether
	// the fleet is spreading or whether every create is being served locally
	// because no candidate would take it. Nil on a host with no metrics.
	Placement Placement
	// Drain empties THIS host on an operator's request, and takes machines
	// another host is emptying. Nil on a host that cannot, which answers 501
	// rather than pretending the route is absent.
	Drain Drainer
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
	// Recipes serves the database fragments `pilot add` and the dashboard
	// splice into a compose file. Injected for the reason Compose is: the
	// generator lives in internal/compose, which imports this package.
	Recipes http.HandlerFunc
	// Plan decides what a directory is and answers with a compose plan.
	// Injected for the same reason Compose is: internal/detect imports this
	// package for the wire structs, so the import cannot go both ways. Nil
	// only in tests, where the route answers 503 rather than vanishing.
	Plan http.HandlerFunc
	// GitHub handles webhook deliveries. Nil when no app is configured, in
	// which case the route answers 503 rather than accepting deliveries it
	// cannot verify.
	GitHub http.HandlerFunc
	// Repos turns a named repository into a build context, through the fleet's
	// GitHub App. Injected for the reason Plan and Compose are: the
	// implementation lives in internal/github, which imports this package.
	//
	// Nil when no app is configured, in which case POST /v1/builds answers
	// not_configured to a JSON body and goes on taking tars. Keep the nil
	// VISIBLE at the call site: a nil *github.Stager assigned here is a
	// non-nil interface, and the 503 branch would never run.
	Repos RepoStager
	// Tenancy answers which org owns an object and whether a key has been
	// revoked, from local state. Nil falls back to the store, which is what a
	// single box wants; a fleet passes the subscription cache so neither
	// question costs a query on the request path.
	Tenancy TenancyView
	// MachineCPU answers how a machine last came up, from local state. Nil
	// falls back to the store; a fleet passes the subscription cache so the
	// field costs a map read rather than a query on every machine read.
	MachineCPU MachineCPUView
	// CPUVendor is this host's pool, reported on /v1/health so an operator can
	// see the fleet's split without a shell on every box. CPUVendorForced says
	// the gate's fault flag is making this host lie about it.
	CPUVendor       string
	CPUVendorForced bool
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
			CPUVendor: d.CPUVendor, CPUVendorForced: d.CPUVendorForced,
		}
		// True by default so a host with no replica (SQLite) is not reported
		// as forever joining. A corrosion host overwrites this below.
		resp.ReplicationComplete = true
		if d.Replication != nil {
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
			if v, vec, complete, err := d.Replication(ctx); err != nil {
				slog.Warn("could not read the store version", "err", err)
				// A replica that cannot be read has not been shown to be
				// caught up, and this field is what a joining peer reads to
				// decide whether IT may act. Unreadable answers as not
				// complete, which is the direction that waits.
				resp.ReplicationComplete = false
			} else {
				resp.StoreVersion, resp.StoreVersions, resp.ReplicationComplete = v, vec, complete
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
	mux.HandleFunc("PATCH /v1/machines/{id}", d.handleUpdateMachine)
	mux.HandleFunc("DELETE /v1/machines/{id}", d.handleDestroyMachine)
	mux.HandleFunc("POST /v1/machines/{id}/exec", d.handleExec)
	mux.HandleFunc("GET /v1/machines/{id}/exec/stream", d.handleExecStream)
	// One TCP connection to a port inside the machine, for `pilot proxy`.
	mux.HandleFunc("GET /v1/machines/{id}/tcp/{port}", d.handleTCPStream)
	// Terminal sessions that outlive their connection: list, attach, end.
	mux.HandleFunc("GET /v1/machines/{id}/sessions", d.handleListSessions)
	mux.HandleFunc("GET /v1/machines/{id}/attach/{session}", d.handleAttachSession)
	mux.HandleFunc("POST /v1/machines/{id}/sessions/{session}/kill", d.handleKillSession)
	// The sprites alias, name-keyed: see handleSpriteExec.
	mux.HandleFunc("GET /v1/sprites/{name}/exec", d.handleSpriteExec)
	mux.HandleFunc("GET /v1/machines/{id}/logs", d.handleLogs)

	// Lifecycle. Suspend/wake are the scale-to-zero pair; stop/start are the
	// non-snapshotting equivalents.
	mux.HandleFunc("POST /v1/machines/{id}/suspend", d.handleSuspend)
	mux.HandleFunc("POST /v1/machines/{id}/wake", d.handleWake)
	// Vertical scaling. A boot rather than a resume, because a memory image
	// cannot be loaded into a machine of another size.
	mux.HandleFunc("POST /v1/machines/{id}/resize", d.handleResizeMachine)
	// Redeploy is the rollout's: the same machine, booted from another image.
	mux.HandleFunc("POST /v1/machines/{id}/redeploy", d.handleRedeploy)
	// stop and start are suspend and wake under the names every other
	// platform's CLI and SDK uses. They were 501 while the CLI and both SDKs
	// already called them, so the commonest lifecycle pair in the product
	// answered "not implemented" on a machine that could do it perfectly well.
	// One behaviour, two spellings, rather than a second mechanism.
	mux.HandleFunc("POST /v1/machines/{id}/stop", d.handleSuspend)
	mux.HandleFunc("POST /v1/machines/{id}/start", d.handleWake)

	// Builders: an org's build machines, and the reset that clears both the
	// wedged one and every host's copy of its layer cache.
	mux.HandleFunc("GET /v1/builders", d.handleListBuilders)
	mux.HandleFunc("POST /v1/builders/{host}/reset", d.handleResetBuilder)

	// Processes: what a machine runs, and how to bounce one of them without
	// touching the others.
	mux.HandleFunc("GET /v1/machines/{id}/processes", d.handleProcesses)
	mux.HandleFunc("POST /v1/machines/{id}/processes/{name}/{action}", d.handleProcessAction)
	mux.HandleFunc("GET /v1/machines/{id}/processes/{name}/logs", d.handleProcessLogs)

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
	// The ONE route that answers with variable VALUES. Every other surface
	// returns names only, on purpose; this one exists so a password that was
	// written can be recovered, rather than kept in a second place that is
	// worse. See serviceenv.go.
	mux.HandleFunc("GET /v1/services/{id}/env", d.handleServiceEnv)
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
			WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
				"no compose planner on this host",
				"this host was built without it; pilot status lists hosts", nil)
		})
	}

	// The front door: a tar in, a plan out. Injected for the same reason the
	// compose plan is, and the two are separate routes because one takes a
	// file's text and the other takes a whole directory.
	if d.Plan != nil {
		mux.HandleFunc("POST /v1/plan", d.Plan)
	} else {
		mux.HandleFunc("POST /v1/plan", func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
				"no planner on this host",
				"this host was built without it; pilot status lists hosts", nil)
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
			WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
				"no github app is configured on this fleet",
				"set PILOT_GITHUB_APP_ID, PILOT_GITHUB_APP_KEY and PILOT_GITHUB_WEBHOOK_SECRET on every host", nil)
		})
	}

	mux.HandleFunc("POST /v1/domains", d.handleAddDomain)
	mux.HandleFunc("GET /v1/domains", d.handleListDomains)
	mux.HandleFunc("DELETE /v1/domains/{hostname}", d.handleDeleteDomain)

	// Volumes and fleet.
	mux.HandleFunc("POST /v1/volumes", d.handleCreateVolume)
	mux.HandleFunc("GET /v1/volumes", d.handleListVolumes)
	// Point-in-time copies. Served by the host that MOUNTS the volume, because
	// a snapshot is a clone inside the volume's own filesystem; a request
	// elsewhere is forwarded there.
	mux.HandleFunc("POST /v1/volumes/{id}/snapshots", d.handleCreateVolumeSnapshot)
	mux.HandleFunc("GET /v1/volumes/{id}/snapshots", d.handleListVolumeSnapshots)
	mux.HandleFunc("POST /v1/volumes/{id}/snapshots/{stamp}/restore", d.handleRestoreVolumeSnapshot)
	// The schedule that takes them without being asked. Nobody takes a manual
	// snapshot before the mistake.
	mux.HandleFunc("DELETE /v1/volumes/{id}/snapshots/{stamp}", d.handleDeleteVolumeSnapshot)
	// A NEW volume from a snapshot, which is what a recovery is built on: it
	// leaves the thing you are recovering from in place to compare against.
	mux.HandleFunc("POST /v1/volumes/{id}/snapshots/{stamp}/fork", d.handleForkVolumeSnapshot)
	mux.HandleFunc("GET /v1/volumes/{id}/policy", d.handleGetVolumePolicy)
	mux.HandleFunc("PUT /v1/volumes/{id}/policy", d.handlePutVolumePolicy)
	// The volume drive as Firecracker holds it, not as hostd meant to set it.
	// See MachineVolume: the difference between the two is a durability
	// guarantee that fails silently.
	mux.HandleFunc("GET /v1/machines/{id}/volume", d.handleMachineVolume)
	mux.HandleFunc("GET /v1/hosts", d.handleListHosts)
	// Every address the acting org's outbound traffic can leave from, which is
	// what a tenant hands to anything that allowlists by source address.
	mux.HandleFunc("GET /v1/egress", d.handleEgress)
	// The compose fragment for a database, with the durability decision made
	// and explained. One generator, fetched by every client, because two
	// copies of a recipe is two places for it to drift from what the planner
	// will accept.
	mux.HandleFunc("GET /v1/recipes/{engine}", d.serveRecipes)
	// Emptying a host on purpose, so a reboot or a retirement is not an outage
	// for the machines it happens to be holding. Admin-scoped: a drain moves
	// every org's machines at once. Any host serves these; the named host does
	// the work, because it is the only one allowed to offer its own machines.
	mux.HandleFunc("POST /v1/hosts/{id}/drain", d.handleDrain)
	mux.HandleFunc("GET /v1/hosts/{id}/drain", d.handleDrainStatus)
	mux.HandleFunc("DELETE /v1/hosts/{id}/drain", d.handleUndrain)
	// Internal: a draining host telling its target to take a machine. The
	// OFFER row authorises the move, so this only saves the target from
	// waiting to notice one.
	mux.HandleFunc("POST /v1/machines/{id}/take", d.handleTake)
	// New machines from an existing one's exact state: the source's processes
	// already running, its memory already warm. A suspended source is forked
	// without waking it.
	mux.HandleFunc("POST /v1/machines/{id}/fork", d.handleForkMachine)
	mux.HandleFunc("POST /v1/checkpoints/{id}/fork", d.handleForkCheckpoint)
	mux.HandleFunc("GET /v1/whoami", d.handleWhoami)

	// The hosted MCP endpoint: the fleet toolset over Streamable HTTP, on
	// every host, behind the same bearer key. See mcp.go. The well-known
	// document beside it is how a client with no key learns where to get one.
	mux.Handle("/mcp", d.mcpHandler())
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", d.handleProtectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", d.handleProtectedResource)

	// Which repositories an org may have this fleet fetch. The connect is
	// admin-scoped inside the handler and the list is not; see repos.go for
	// why the two halves differ.
	mux.HandleFunc("POST /v1/repos", d.handleConnectRepo)
	mux.HandleFunc("GET /v1/repos", d.handleListRepos)

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
	WriteError(w, http.StatusNotImplemented, CodeNotImplemented, "not implemented",
		"this route is not built yet; the CLI and the SDKs never call it", nil)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// RepoStager fetches a repository at a ref and answers with a build context
// tar, recording anything it refuses under the given build id so a person
// reads the reason at GET /v1/builds/{id}/logs.
//
// The caller closes the reader. A refusal comes back as *Refusal, which is why
// that type lives in this package.
type RepoStager interface {
	Context(ctx context.Context, id, repo, ref, app string) (io.ReadCloser, error)
}

// Sealer seals a secret environment, and opens one again. Satisfied by
// seal.Key.
//
// Open is here for the depends_on derivation and nothing else: a real
// database URL is a sealed value, so an app whose services find each other
// through it would draw no edges at all from the plaintext half. Nothing Open
// returns may leave the caller. It is scanned for <name>.internal references
// and dropped; no plaintext is returned in a response, written to a row, or
// logged, and a host with no key derives from the plaintext half instead.
type Sealer interface {
	IsSet() bool
	Seal([]byte) (string, error)
	Open(blob string) ([]byte, error)
}
