// Package api is hostd's public HTTP surface.
//
// Every host serves this identical API -- there is no control-plane tier, and
// no request path may require a specific host to be alive. The CLI, both SDKs,
// the MCP server, and the dashboard are all written against these JSON tags,
// so the wire shapes here are a contract: change them deliberately, not
// incidentally.
package api

import (
	"encoding/json"
	"fmt"
)

// Knobs are the per-machine lifecycle policy. There is no sandbox type and no
// service type -- a sandbox and a production service are the same machine with
// different knobs. Scale-to-zero (MinMachinesRunning == 0) is valid for
// production services, exactly as it is on Fly.
type Knobs struct {
	AutoStop           string `json:"auto_stop"`            // off|stop|suspend
	AutoStart          bool   `json:"auto_start"`           // wake on an inbound request
	MinMachinesRunning int    `json:"min_machines_running"` // 0 = scale to zero
	SoftLimit          int    `json:"soft_limit"`           // concurrency before starting another replica
}

// DefaultKnobs is the policy a machine gets when the caller says nothing.
//
// The defaults keep a machine REACHABLE and cheap: it suspends when idle and
// wakes on the next request.
func DefaultKnobs() Knobs {
	return Knobs{AutoStop: "suspend", AutoStart: true, SoftLimit: 20}
}

// DecodeKnobs applies a caller's partial policy on top of the defaults.
//
// Decoding onto a pre-seeded value is the point: encoding/json leaves absent
// fields untouched, so {"auto_stop":"suspend"} keeps auto_start true. Assigning
// a decoded struct wholesale instead would zero every field the caller did not
// mention -- and a machine with auto_start false suspends after a minute and
// then refuses to wake, which is a permanently dead URL earned by setting one
// unrelated field.
func DecodeKnobs(raw json.RawMessage) (Knobs, error) {
	k := DefaultKnobs()
	if len(raw) == 0 {
		return k, nil
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return k, fmt.Errorf("api: invalid knobs: %w", err)
	}
	return k, nil
}

// ParseKnobs reads a machine's stored policy.
//
// The stored blob is exactly this struct serialised, so there is no
// translation layer between the wire format and what is persisted. Defaults
// keep a machine REACHABLE: a corrupt or missing value must not strand it with
// autoStart off.
func ParseKnobs(raw string) Knobs {
	k := DefaultKnobs()
	if raw == "" {
		return k
	}
	_ = json.Unmarshal([]byte(raw), &k)
	return k
}

// MarshalKnobs serialises a machine's policy for storage.
func MarshalKnobs(k Knobs) (string, error) {
	raw, err := json.Marshal(k)
	if err != nil {
		return "", fmt.Errorf("api: marshal knobs: %w", err)
	}
	return string(raw), nil
}

// Machine is the platform's one primitive: a Firecracker microVM whose
// identity -- id, name, URL, agent token -- never changes for its whole life,
// across suspend, wake, checkpoint, restore, promote, and host migration.
type Machine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HostID string `json:"host_id"`
	// OrgID is the tenant that owns it. Absent on a row created before
	// tenancy existed, which only an admin key can see at all.
	OrgID        string `json:"org_id,omitempty"`
	State        string `json:"state"` // creating|running|suspended|stopped|error
	Knobs        Knobs  `json:"knobs"`
	ImageRef     string `json:"image_ref,omitempty"`
	VCPUs        int    `json:"vcpus"`
	MemMiB       int    `json:"mem_mib"`
	URL          string `json:"url"`                     // <scheme>://<name>.<workload domain>[:port]; https and portless on a TLS host
	CustomDomain string `json:"custom_domain,omitempty"` // services only
	VolumeID     string `json:"volume_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	ReleaseID    string `json:"release_id,omitempty"`
	App          string `json:"app,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	LastActivity int64  `json:"last_activity"`
	// LastStart says how this machine last came up: "restore" of a memory
	// image, the "boot" a create with an image or a volume pays once (and every
	// redeploy), or "cold_boot", a restore DOWNGRADED because no host of the
	// image's CPU vendor was alive. A cold boot keeps URL, disk and volume and
	// loses memory; this field is how a client tells it from a resume.
	LastStart   string `json:"last_start,omitempty"`
	LastStartAt int64  `json:"last_start_at,omitempty"`
}

// CreateMachineRequest creates a machine from exactly one source: a built
// image, a template, or a checkpoint. Creating from a template is a restore,
// not a boot -- that is what makes create instant.
type CreateMachineRequest struct {
	Name       string `json:"name,omitempty"` // generated when empty
	Image      string `json:"image,omitempty"`
	Template   string `json:"template,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	VCPUs      int    `json:"vcpus,omitempty"`
	MemMiB     int    `json:"mem_mib,omitempty"`
	// Raw so a partial object merges onto the defaults instead of
	// replacing them. See DecodeKnobs.
	Knobs  json.RawMessage `json:"knobs,omitempty"`
	Volume string          `json:"volume,omitempty"`

	// App groups machines that may find and reach each other by name. Grouping
	// only: there is no apps table, because an app is a property of the
	// client's compose file rather than a fleet object.
	App string `json:"app,omitempty"`

	// Cmd is the application this machine runs, as a shell command. It is
	// written inside the guest and started after the environment is delivered
	// -- the golden template deliberately stops short of starting anything,
	// because a create is a resume and a running process cannot be handed an
	// environment it did not start with.
	Cmd string `json:"cmd,omitempty"`

	// MemBuildID and RootfsBuildID create a machine by RESTORING a build pair
	// rather than booting an image -- how every replica of a release after the
	// first comes up, and what makes a deploy land on the measured sub-second
	// path instead of a cold boot. Internal: set by the rollout, not by a
	// client, and ignored unless both are present. A client that names a
	// pair is held to the same ownership check as Image, and a memory build
	// never has an owner row, so the pair is admin-only on the API.
	MemBuildID    string `json:"mem_build_id,omitempty"`
	RootfsBuildID string `json:"rootfs_build_id,omitempty"`

	// Service and Release record which service's rollout this machine belongs
	// to, so a deploy can find its own replicas and a rollback can find the
	// previous ones.
	Service string `json:"service,omitempty"`
	Release string `json:"release,omitempty"`

	// Env is the non-secret environment, stored as-is.
	Env map[string]string `json:"env,omitempty"`

	// SecretEnv is the secret half, sent in PLAINTEXT over TLS and sealed by
	// hostd with the fleet key before any row is written.
	//
	// secret:// references are resolved CLIENT-side, before a request is
	// built, so the value never enters a repository. And the sealing happens
	// HERE rather than in the client: a client that sealed would need the
	// fleet key, and a key on every laptop is not fleet infrastructure.
	SecretEnv map[string]string `json:"secret_env,omitempty"`

	// OrgID is the tenant this machine belongs to, filled from the
	// authenticated key. `json:"-"` is load-bearing: a client that could set
	// it in the body could create machines inside another tenant.
	OrgID string `json:"-"`
}

// ExecRequest runs a command inside a machine. Cwd and Env are required on
// every exec by the reference AI-agent workload, buffered and streaming alike.
type ExecRequest struct {
	Cmd       string            `json:"cmd"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	User      string            `json:"user,omitempty"` // defaults to uid 1000
	TimeoutMS int               `json:"timeout_ms,omitempty"`
}

type ExecResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Exec stream frame prefixes. The WebSocket exec stream sends binary frames
// whose first byte is one of these; for FrameExit the payload's first byte is
// the exit code. This is byte-compatible with the sprites protocol on purpose,
// so existing clients drop in unchanged.
const (
	// Client to server; honoured only when the stream was opened with stdin=true.
	// hostd sends a text {"type":"exit","exit_code":n} after the binary exit.
	FrameStdin    byte = 0
	FrameStdinEOF byte = 4

	FrameStdout byte = 1
	FrameStderr byte = 2
	FrameExit   byte = 3
)

// The tty contract on the exec stream.
//
// tty=true runs the command on a pseudo-terminal, which is what an interactive
// shell, tmux and vim need and what three pipes cannot give them. It is a mode
// on this stream rather than a second route on purpose: one protocol, one set
// of frames, one exit verdict.
//
// Query: tty=true, plus rows and cols for the initial window (24 by 80 by
// default, each 1..65535; anything else closes the socket with 1008). tty=true
// with stdin=false is a 400 here, before the machine is woken.
//
// Under tty, and only under tty:
//
//   - a PTY merges the two output streams, so everything arrives as FrameStdout
//     and FrameStderr is NEVER sent;
//   - client frames are read whether or not stdin=true was passed, because a
//     terminal implies stdin;
//   - FrameStdinEOF writes EOT (0x04) to the terminal instead of closing an
//     input, because a terminal has no separate write end to close, and the
//     session stays open;
//   - a TEXT message {"type":"resize","cols":N,"rows":N} resizes the window.
//     It is a control message, not a frame: no byte id is spent on it.
//
// Everything else is unchanged, the exit verdict included, so a client that
// only reads frames needs no tty-specific code to learn how a command ended.

// CheckpointRequest names a restorable point. Checkpoints chain: restoring an
// older one discards writes made after it.
type CheckpointRequest struct {
	Comment string `json:"comment,omitempty"`
}

// Checkpoint and a release are the same artifact, which is why promote and
// rollback are the same operation underneath.
type Checkpoint struct {
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	Seq       int    `json:"seq"`
	Comment   string `json:"comment,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	Durable   bool   `json:"durable"` // false = upload still in flight
	CreatedAt int64  `json:"created_at"`

	// ResumeGapMS is how long the guest was frozen, in milliseconds. Present
	// only on the response that created the checkpoint. The call itself takes
	// longer: the preparation before the pause runs with the machine serving.
	ResumeGapMS int64 `json:"resume_gap_ms,omitempty"`
}

// BuildLogLine is one NDJSON line of a streamed build. Structured so an agent
// can parse a failure, patch the Dockerfile, and retry without a human.
type BuildLogLine struct {
	Step   string `json:"step,omitempty"`
	Stream string `json:"stream,omitempty"` // stdout|stderr|status
	Line   string `json:"line,omitempty"`
	TS     int64  `json:"ts"`
	Error  string `json:"error,omitempty"`
	Result string `json:"result,omitempty"` // rootfs_build_id on success
	// Code is the stable code on a terminal failure line, the same closed
	// list ErrorResponse.Code draws from, so a consumer that reads the log
	// instead of the status branches on the same value.
	Code string `json:"code,omitempty"`
}

// HealthCheck gates a rollout: a new release takes traffic only once healthy.
//
// A tagged union rather than an HTTP path, because a database ships a command
// check and not an endpoint, and every stock image already declares one. The
// shape is Docker's, so an image's own HEALTHCHECK maps straight in.
//
//	{"type":"http","path":"/__webjs/ready","grace":40,"healthy_threshold":2}
//	{"type":"cmd","test":["CMD-SHELL","pg_isready -U postgres"],"retries":5}
type HealthCheck struct {
	Type string `json:"type,omitempty"` // "http" (default) | "cmd" | "none"
	Path string `json:"path,omitempty"`
	// Test is Docker's form: ["CMD-SHELL", "..."], ["CMD", argv...], ["NONE"].
	Test []string `json:"test,omitempty"`

	// Seconds, matching Docker and the Dockerfile HEALTHCHECK this parses from.
	IntervalSec      int `json:"interval,omitempty"`
	TimeoutSec       int `json:"timeout,omitempty"`
	GraceSec         int `json:"grace,omitempty"`
	HealthyThreshold int `json:"healthy_threshold,omitempty"`
}

type Service struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	OrgID string `json:"org_id,omitempty"`
	// App groups services that may find each other by <name>.internal.
	App string `json:"app,omitempty"`
	// DependsOn names the sibling services in this app whose <name>.internal
	// address this service's environment references.
	//
	// DERIVED on every read, from both halves of the environment, and stored
	// nowhere. There is no depends_on column and there will not be one:
	// adding a column to a populated Corrosion table backfills and gossips
	// every existing row, which is hard rule 6, and a declared dependency
	// would then need a dual read to be believed.
	//
	// A NAME, never a value. "web dials db" is what the app grouping and a
	// connection attempt already say out loud, so nothing here is a secret
	// the caller could not have learned by reading its own compose file.
	DependsOn    []string     `json:"depends_on,omitempty"`
	Replicas     int          `json:"replicas"`
	Knobs        Knobs        `json:"knobs"`
	Health       *HealthCheck `json:"health,omitempty"`
	URL          string       `json:"url,omitempty"`
	CustomDomain string       `json:"custom_domain,omitempty"`
	ReleaseID    string       `json:"release_id,omitempty"`
	// VolumeID is the volume every replica of this service mounts. A service
	// with one runs one replica, because a volume is mounted by exactly one
	// machine.
	VolumeID   string `json:"volume_id,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Autodeploy bool   `json:"autodeploy"`
	CreatedAt  int64  `json:"created_at"`
}

type CreateServiceRequest struct {
	Name     string `json:"name"`
	App      string `json:"app,omitempty"`
	Release  string `json:"release,omitempty"`
	Build    string `json:"build,omitempty"`
	Replicas int    `json:"replicas,omitempty"`
	// Knobs is accepted for wire compatibility and not persisted: a service
	// row has no knobs column and the first replica does not exist until the
	// first deploy. The knobs a service runs with are the ones on
	// DeployRequest.
	Knobs  *Knobs       `json:"knobs,omitempty"`
	Health *HealthCheck `json:"health,omitempty"`
	// Domain is the subdomain label under the fleet's domain. Empty means the
	// service mints no route rows -- legal, and reachable by peers over
	// <name>.internal instead.
	Domain       string `json:"domain,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	// Volume is create-only: a volume swap is a data migration, not a
	// configuration change, and nothing here copies data between volumes, so
	// the update route does not take it. Requires replicas of at most one.
	Volume string `json:"volume,omitempty"`

	Env map[string]string `json:"env,omitempty"`
	// SecretEnv is plaintext over TLS and sealed by hostd with the fleet key
	// before any row is written -- never by the client, which would need the
	// fleet key to do it.
	SecretEnv map[string]string `json:"secret_env,omitempty"`

	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Autodeploy bool   `json:"autodeploy,omitempty"`

	// OrgID comes from the authenticated key, never from the body. See
	// CreateMachineRequest.OrgID.
	OrgID string `json:"-"`
}

// RepoRef is the JSON body POST /v1/plan and POST /v1/builds accept in place
// of a tar: a repository the fleet's GitHub App is installed on, at a ref.
// The host fetches the bytes through the path a push takes.
//
// So that no client has to hold the repository. A browser session holds an
// App JWT and nothing else, and a tar it staged itself would be a second copy
// of the fetch, the root strip and the recipe rules, with 2 GiB uploads
// transiting a tenant-facing process.
type RepoRef struct {
	Repo string `json:"repo"` // owner/name
	Ref  string `json:"ref"`  // branch, tag or sha
}

// DeployRequest cuts a new release over, health-gated, keeping the previous
// release available for rollback.
type DeployRequest struct {
	Release string `json:"release,omitempty"`
	Build   string `json:"build,omitempty"`
	// Knobs is the lifecycle policy for the replicas this deploy creates,
	// partial and merged onto what the previous release's replicas carry (or
	// the machine defaults for a first deploy), exactly as
	// CreateMachineRequest.Knobs is merged. A service row keeps no knobs, so
	// the deploy is where they travel: {"min_machines_running":1} is how a
	// replica is kept warm, and a redeploy with different knobs changes them.
	Knobs json.RawMessage `json:"knobs,omitempty"`
}

// PromoteRequest turns a sandbox into a durable service. The machine's URL is
// unchanged by promotion; a custom domain is additive.
type PromoteRequest struct {
	CustomDomain string       `json:"custom_domain,omitempty"`
	Replicas     int          `json:"replicas,omitempty"`
	Health       *HealthCheck `json:"health,omitempty"`
}

// RedeployRequest boots a machine again from another image, in place: same
// row, same URL, same volume. How a volume-backed service takes a release,
// because a second machine cannot mount the volume beside the first.
// Internal: the rollout sends it, here or to the host holding the machine.
type RedeployRequest struct {
	Image   string `json:"image"`
	Release string `json:"release,omitempty"`
}

// UpdateServiceRequest patches a service.
//
// Pointer fields so an absent value is distinguishable from a zero one: the
// dashboard disconnects a repo by sending repo: "", which only a pointer can
// carry. Env and SecretEnv REPLACE the stored map rather than merging into it,
// so a client that wants a merge does it client-side and sends the result.
//
// Env, SecretEnv and Replicas take effect at the NEXT DEPLOY, which is where a
// rollout reads them. No knobs: a service row has no knobs column, and replica
// rows are single-writer to their own hosts, so the arbiter could not apply
// them if it had them. They travel on the deploy, and a body carrying one is a
// 400 naming the field.
type UpdateServiceRequest struct {
	Replicas   *int              `json:"replicas,omitempty"`
	Health     *HealthCheck      `json:"health,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	SecretEnv  map[string]string `json:"secret_env,omitempty"`
	Repo       *string           `json:"repo,omitempty"`
	Branch     *string           `json:"branch,omitempty"`
	Autodeploy *bool             `json:"autodeploy,omitempty"`
}

// Volume is persistent, per-write-durable storage: one filesystem in object
// storage holding one disk image, handed to a machine as a second drive.
//
// It is attached to at most one machine and mounted by at most one host, and
// both of those are reported rather than inferred -- a volume that two hosts
// believe they hold is not recoverable, so an operator has to be able to see
// where it is.
type Volume struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OrgID     string `json:"org_id,omitempty"`
	SizeGiB   int    `json:"size_gib"`
	MachineID string `json:"machine_id,omitempty"`
	HostID    string `json:"host_id,omitempty"`
	MountPath string `json:"mount_path"`
	CreatedAt int64  `json:"created_at"`
}

// MachineVolume reports the volume drive a running machine actually has.
//
// CacheType is read back out of Firecracker rather than repeated from what
// hostd meant to configure. That is the entire reason this shape exists: the
// default cache type does not advertise the VirtIO flush feature, so a guest
// fsync on a drive left at the default returns success with the data only in
// the host's page cache -- and the intent and the reality can differ with
// nothing anywhere to notice.
type MachineVolume struct {
	VolumeID  string `json:"volume_id"`
	MountPath string `json:"mount_path"`
	Device    string `json:"device"`
	CacheType string `json:"cache_type"`
}

type CreateVolumeRequest struct {
	Name    string `json:"name"`
	SizeGiB int    `json:"size_gib"`
	// MountPath is where the guest mounts it. Defaults to /data.
	MountPath string `json:"mount_path,omitempty"`

	// OrgID comes from the authenticated key, never from the body. See
	// CreateMachineRequest.OrgID.
	OrgID string `json:"-"`
}

// Host is one member of the fleet, as seen by any host reading its local
// replica of cluster state.
type Host struct {
	ID         string `json:"id"`
	PublicIP   string `json:"public_ip,omitempty"`
	WGAddr     string `json:"wg_addr,omitempty"`
	CPUFree    int    `json:"cpu_free"`
	MemFreeMiB int    `json:"mem_free_mib"`
	LastSeen   int64  `json:"last_seen"`
	Alive      bool   `json:"alive"`
	// CPUVendor is which pool this host restores memory images from, the raw
	// /proc/cpuinfo vendor_id. Empty on a host that has not published its row
	// yet, which ranks as "in no pool".
	CPUVendor string `json:"cpu_vendor,omitempty"`
}

// CreateAPIKeyRequest mints a key for an org. Admin-scoped: the org is named
// in the body because an admin acts across orgs, unlike every other create on
// this API, where the org comes from the key and never from the body.
type CreateAPIKeyRequest struct {
	OrgID  string   `json:"org_id"`
	Scopes []string `json:"scopes"`
}

// APIKeyResponse carries the plaintext key exactly once, on the mint. Every
// later read of the same row -- the list -- leaves Key empty, because the
// plaintext was never stored to read back.
type APIKeyResponse struct {
	Key       string   `json:"key,omitempty"`
	Hash      string   `json:"hash"`
	OrgID     string   `json:"org_id"`
	Scopes    []string `json:"scopes"`
	CreatedAt int64    `json:"created_at"`
	RevokedAt int64    `json:"revoked_at,omitempty"`
}

type RevokeResponse struct {
	Hash      string `json:"hash"`
	RevokedAt int64  `json:"revoked_at"`
}

// ConnectRepoRequest ties a repository to an org, which is what lets that
// org's own keys name it in a {repo, ref} build or plan.
//
// The org is NOT in the body. It comes from the key, or from ?org= on an admin
// key, exactly as it does on every other create -- naming it twice would be
// two answers to one question, and the one in the body would be the one no
// scope check ever saw.
type ConnectRepoRequest struct {
	Repo string `json:"repo"` // owner/name
}

// RepoLinkResponse is one connection, as GET /v1/repos lists it and as
// POST /v1/repos echoes it back.
type RepoLinkResponse struct {
	Repo        string `json:"repo"`
	OrgID       string `json:"org_id"`
	ConnectedAt int64  `json:"connected_at"`
}

// RepoLinkListResponse is never null, so a client rendering "no repositories
// connected" does not have to tell an empty fleet from a broken one.
type RepoLinkListResponse struct {
	Repos []RepoLinkResponse `json:"repos"`
}

// QuotaResponse is one org's limits, and also the PUT body minus updated_at.
type QuotaResponse struct {
	OrgID        string `json:"org_id"`
	MaxMachines  int    `json:"max_machines"`
	MaxVCPUs     int    `json:"max_vcpus"`
	MaxMemMiB    int    `json:"max_mem_mib"`
	MaxVolumeGiB int    `json:"max_volume_gib"`
	MaxBuilds    int    `json:"max_builds"`
	UpdatedAt    int64  `json:"updated_at,omitempty"`
}

// QuotaExceededResponse names the limit that refused a request, so a client is
// told what to raise rather than only that something was too big.
type QuotaExceededResponse struct {
	Error string `json:"error"`
	// Code and Next mirror ErrorResponse so that a client branching on the
	// error shape does not need a second one for this route's body.
	Code  string `json:"code"`
	Next  string `json:"next"`
	Quota string `json:"quota"`
	Limit int    `json:"limit"`
	Used  int    `json:"used"`
	// Scope is "host" when the limit is per host rather than fleet-wide,
	// which is true of builds alone: a build is not a replicated object.
	Scope string `json:"scope,omitempty"`
}

// UsageTotals is one org's accrual on this host over the requested range.
//
// Compute (vcpu_seconds, mib_seconds) accrues only while a machine is running;
// a suspended machine bills storage only, which is machine_seconds and
// volume_gib_seconds. See internal/usage for the accrual rule in full.
type UsageTotals struct {
	MachineSeconds   int64 `json:"machine_seconds"`
	VCPUSeconds      int64 `json:"vcpu_seconds"`
	MiBSeconds       int64 `json:"mib_seconds"`
	VolumeGiBSeconds int64 `json:"volume_gib_seconds"`
}

// UsageResponse is what THIS host metered, never the fleet's total: there is
// no aggregator tier, so the dashboard polls every live host and sums. Orgs is
// never null, because a client that exports a CSV of it would otherwise have
// to distinguish "no usage" from "broken".
type UsageResponse struct {
	HostID string                 `json:"host_id"`
	Since  int64                  `json:"since"`
	Until  int64                  `json:"until"`
	Orgs   map[string]UsageTotals `json:"orgs"`
}

type HealthResponse struct {
	OK     bool   `json:"ok"`
	HostID string `json:"host_id"`
	// Reflink reports whether this host's machine store can share extents.
	// Without it the engine still works and still passes every correctness
	// assertion, but create and checkpoint are several times slower, because
	// image copies that should be metadata operations become real ones. It is
	// on the health response so that a degraded host is visible from the
	// outside rather than only in a latency graph nobody is watching.
	Reflink bool `json:"reflink"`
	// HugePages reports whether guest memory on this host is backed by 2MiB
	// pages. Unlike Reflink this is not only a speed signal: the page size is
	// recorded in every snapshot and cannot be reinterpreted at restore, so a
	// host that disagrees with the fleet cannot restore the fleet's machines
	// at all. It is also what capacity is counted from, since reserved
	// hugepages do not appear in MemAvailable.
	HugePages bool `json:"hugepages"`
	// StoreVersion is the sum of the local replica's version vector: how many
	// changes, from every host, this replica has applied. 0 on SQLite. Two
	// hosts far apart on this number are a replication problem before they
	// are anything else.
	StoreVersion int64 `json:"store_version"`
	// CPUVendor is this host's vendor pool, the raw /proc/cpuinfo vendor_id.
	// A memory image never restores across the Intel/AMD boundary, so this is
	// what says which of the fleet's snapshots this host can load.
	CPUVendor string `json:"cpu_vendor"`
	// CPUVendorForced is true when PILOT_FAULT_CPU_VENDOR is making this host
	// lie about its CPU. It exists so the fleet gate can prove it armed the
	// fault rather than assume it; a real host never sets it.
	CPUVendorForced bool `json:"cpu_vendor_forced,omitempty"`
}

// WhoamiResponse is what the caller's key resolves to on the host that
// answered. The org is empty for the bootstrap admin key, which belongs to no
// org, and the CLI renders that case as "(admin key, no org)" rather than as a
// missing value.
type WhoamiResponse struct {
	OrgID  string   `json:"org_id"`
	Scopes []string `json:"scopes"`
	HostID string   `json:"host_id"`
}

// ErrorResponse is every non-2xx body. Code is a stable snake_case noun a
// client branches on; Next is the one thing to do about it, naming the
// command or the call; Details is typed per code (HealthGateDetails,
// compose.UnknownDetails) and absent otherwise.
//
// Three fields rather than one sentence because the consumer is as often an
// agent as a person: a sentence has to be parsed to be acted on, and a parser
// written against prose breaks the first time the prose is improved.
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Next    string `json:"next,omitempty"`
	Details any    `json:"details,omitempty"`
}

// HealthGateDetails is the 422 body's details and the error the rollout
// returns, one type so nothing is copied between them. It never carries an
// address: the host-internal probe target is not something a caller can reach,
// and printing it has only ever sent people to debug a 10.x address that is
// not routable from where they are reading it.
type HealthGateDetails struct {
	Service  string     `json:"service"`
	Replica  string     `json:"replica"`
	Release  string     `json:"release"`
	GraceSec int        `json:"grace_sec"`
	Last     HealthLast `json:"last"`
}

// HealthLast is the replica's last answer: a status and body when it
// answered, or a one-line reason when it did not.
type HealthLast struct {
	Status int    `json:"status,omitempty"`
	Body   string `json:"body,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Error reads "replica <r> of <s> did not become healthy within <n>s: <last>",
// where last is the error line or "last answer was <status> <body>".
func (e *HealthGateDetails) Error() string {
	last := e.Last.Error
	if last == "" {
		last = fmt.Sprintf("last answer was %d %s", e.Last.Status, e.Last.Body)
	}
	of := ""
	if e.Service != "" {
		of = " of " + e.Service
	}
	return fmt.Sprintf("replica %s%s did not become healthy within %ds: %s",
		e.Replica, of, e.GraceSec, last)
}
