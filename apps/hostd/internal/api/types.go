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
	ID           string `json:"id"`
	Name         string `json:"name"`
	HostID       string `json:"host_id"`
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
type HealthCheck struct {
	Path        string `json:"path,omitempty"`
	IntervalMS  int    `json:"interval_ms,omitempty"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
	GracePeriod int    `json:"grace_period_ms,omitempty"`
}

type Service struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Replicas     int          `json:"replicas"`
	Knobs        Knobs        `json:"knobs"`
	Health       *HealthCheck `json:"health,omitempty"`
	CustomDomain string       `json:"custom_domain,omitempty"`
	ReleaseID    string       `json:"release_id,omitempty"`
	CreatedAt    int64        `json:"created_at"`
}

type CreateServiceRequest struct {
	Name         string       `json:"name"`
	Release      string       `json:"release,omitempty"`
	Build        string       `json:"build,omitempty"`
	Replicas     int          `json:"replicas,omitempty"`
	Knobs        *Knobs       `json:"knobs,omitempty"`
	Health       *HealthCheck `json:"health,omitempty"`
	CustomDomain string       `json:"custom_domain,omitempty"`
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
	SizeGiB   int    `json:"size_gib"`
	MachineID string `json:"machine_id,omitempty"`
	HostID    string `json:"host_id,omitempty"`
	MountPath string `json:"mount_path"`
	CreatedAt int64  `json:"created_at"`
}

type CreateVolumeRequest struct {
	Name    string `json:"name"`
	SizeGiB int    `json:"size_gib"`
	// MountPath is where the guest mounts it. Defaults to /data.
	MountPath string `json:"mount_path,omitempty"`
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
}

type ErrorResponse struct {
	Error string `json:"error"`
}
