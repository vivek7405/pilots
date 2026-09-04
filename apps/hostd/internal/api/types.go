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
	URL          string `json:"url"`                     // https://<name>.pilotrun.app
	CustomDomain string `json:"custom_domain,omitempty"` // services only
	VolumeID     string `json:"volume_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	ReleaseID    string `json:"release_id,omitempty"`
	App          string `json:"app,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	LastActivity int64  `json:"last_activity"`
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
	// client, and ignored unless both are present.
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
	FrameStdout byte = 1
	FrameStderr byte = 2
	FrameExit   byte = 3
)

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
	App          string       `json:"app,omitempty"`
	Replicas     int          `json:"replicas"`
	Knobs        Knobs        `json:"knobs"`
	Health       *HealthCheck `json:"health,omitempty"`
	URL          string       `json:"url,omitempty"`
	CustomDomain string       `json:"custom_domain,omitempty"`
	ReleaseID    string       `json:"release_id,omitempty"`
	Repo         string       `json:"repo,omitempty"`
	Branch       string       `json:"branch,omitempty"`
	Autodeploy   bool         `json:"autodeploy"`
	CreatedAt    int64        `json:"created_at"`
}

type CreateServiceRequest struct {
	Name     string       `json:"name"`
	App      string       `json:"app,omitempty"`
	Release  string       `json:"release,omitempty"`
	Build    string       `json:"build,omitempty"`
	Replicas int          `json:"replicas,omitempty"`
	Knobs    *Knobs       `json:"knobs,omitempty"`
	Health   *HealthCheck `json:"health,omitempty"`
	// Domain is the subdomain label under the fleet's domain. Empty means the
	// service mints no route rows -- legal, and reachable by peers over
	// <name>.internal instead.
	Domain       string `json:"domain,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`

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

// DeployRequest cuts a new release over, health-gated, keeping the previous
// release available for rollback.
type DeployRequest struct {
	Release string `json:"release,omitempty"`
	Build   string `json:"build,omitempty"`
}

// PromoteRequest turns a sandbox into a durable service. The machine's URL is
// unchanged by promotion; a custom domain is additive.
type PromoteRequest struct {
	CustomDomain string       `json:"custom_domain,omitempty"`
	Replicas     int          `json:"replicas,omitempty"`
	Health       *HealthCheck `json:"health,omitempty"`
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
	Quota string `json:"quota"`
	Limit int    `json:"limit"`
	Used  int    `json:"used"`
	// Scope is "host" when the limit is per host rather than fleet-wide,
	// which is true of builds alone: a build is not a replicated object.
	Scope string `json:"scope,omitempty"`
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
}

type ErrorResponse struct {
	Error string `json:"error"`
}
