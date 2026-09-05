package pilots

// The wire contract, mirrored from apps/hostd/internal/api and
// apps/hostd/internal/compose.
//
// One struct per hostd struct, same name and same JSON tags; the structs from
// the compose package carry a Compose prefix, because Plan, Step and Request
// are far too generic to export unqualified. types_drift_test.go parses
// hostd's Go source on every run and fails naming the struct and the tag when
// the two sides disagree, in either direction.

// Knobs is the per-machine lifecycle policy. A sandbox and a production
// service are the same machine with different knobs.
type Knobs struct {
	AutoStop           string `json:"auto_stop"`            // off|stop|suspend
	AutoStart          bool   `json:"auto_start"`           // wake on an inbound request
	MinMachinesRunning int    `json:"min_machines_running"` // 0 = scale to zero
	SoftLimit          int    `json:"soft_limit"`
}

// KnobsPatch is a PARTIAL lifecycle policy: the shape a REQUEST carries.
//
// hostd decodes a request's knobs onto a value it has already seeded -- its
// own defaults on a create, the sibling replica's policy on a deploy -- so a
// field the caller leaves out keeps the value it had. Expressing that needs a
// per-field "absent", and *Knobs cannot say it: Knobs carries no omitempty
// (responses always spell all four), so &Knobs{SoftLimit: 50} serialises all
// four and the three nobody mentioned are merged as zeros.
//
// That is not a cosmetic difference. It lands auto_start false, and a machine
// with auto_start false suspends after a minute and is then refused its wake
// -- a permanently dead URL earned by raising a concurrency limit. In the
// other direction &Knobs{MinMachinesRunning: 1} silently drops soft_limit to
// 0 and concurrency scale-up stops.
//
// So every field here is a pointer. Nil is absent; a pointer to the zero
// value is a deliberate zero, which is what makes min_machines_running: 0
// (scale to zero) and auto_start: false sayable at all -- an omitempty value
// type could not say either. Fill one in with Ptr:
//
//	pilots.KnobsPatch{SoftLimit: pilots.Ptr(50)}
type KnobsPatch struct {
	AutoStop           *string `json:"auto_stop,omitempty"`            // off|stop|suspend
	AutoStart          *bool   `json:"auto_start,omitempty"`           // wake on an inbound request
	MinMachinesRunning *int    `json:"min_machines_running,omitempty"` // 0 = scale to zero
	SoftLimit          *int    `json:"soft_limit,omitempty"`
}

// Ptr returns a pointer to v, so a KnobsPatch field can be set inline.
func Ptr[T any](v T) *T { return &v }

// Machine is the one primitive. Its id, name and URL never change, across
// suspend, wake, checkpoint, restore, promote and host migration.
type Machine struct {
	// OrgID is the org that owns this object. Set when the caller is an admin
	// key, which is the only caller that sees objects across orgs; a
	// tenant-scoped key only ever sees its own.
	OrgID        string `json:"org_id,omitempty"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	HostID       string `json:"host_id"`
	State        string `json:"state"` // creating|running|suspended|stopped|error
	Knobs        Knobs  `json:"knobs"`
	ImageRef     string `json:"image_ref,omitempty"`
	VCPUs        int    `json:"vcpus"`
	MemMiB       int    `json:"mem_mib"`
	URL          string `json:"url"`
	CustomDomain string `json:"custom_domain,omitempty"`
	VolumeID     string `json:"volume_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	ReleaseID    string `json:"release_id,omitempty"`
	App          string `json:"app,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	LastActivity int64  `json:"last_activity"`
}

// CreateMachineRequest creates a machine from exactly one source: a built
// image, a template, or a checkpoint.
type CreateMachineRequest struct {
	Name       string `json:"name,omitempty"` // generated when empty
	Image      string `json:"image,omitempty"`
	Template   string `json:"template,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	VCPUs      int    `json:"vcpus,omitempty"`
	MemMiB     int    `json:"mem_mib,omitempty"`
	// A patch, not a policy: hostd merges what is present onto its defaults,
	// so the fields left nil keep theirs. See KnobsPatch for why a *Knobs
	// here mints a dead URL.
	Knobs  *KnobsPatch `json:"knobs,omitempty"`
	Volume string      `json:"volume,omitempty"`
	App    string      `json:"app,omitempty"`
	Cmd    string      `json:"cmd,omitempty"`
	// Set by a rollout, not by a client.
	MemBuildID    string            `json:"mem_build_id,omitempty"`
	RootfsBuildID string            `json:"rootfs_build_id,omitempty"`
	Service       string            `json:"service,omitempty"`
	Release       string            `json:"release,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	// SecretEnv travels in plaintext over TLS and is sealed by hostd with the
	// fleet key before any row is written. secret:// references are resolved
	// client-side, before the request is built.
	SecretEnv map[string]string `json:"secret_env,omitempty"`
}

// ExecRequest runs a command inside a machine, buffered.
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

// Exec stream frame prefixes, byte-compatible with the sprites protocol.
// The first byte of every binary frame is one of these; for FrameExit the
// payload's first byte is the exit code.
const (
	FrameStdin    byte = 0
	FrameStdout   byte = 1
	FrameStderr   byte = 2
	FrameExit     byte = 3
	FrameStdinEOF byte = 4
)

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
	// ResumeGapMS is how long the guest was frozen. Present only on the
	// response that created the checkpoint.
	ResumeGapMS int64 `json:"resume_gap_ms,omitempty"`
}

// BuildLogLine is one NDJSON line of a streamed build.
type BuildLogLine struct {
	Step   string `json:"step,omitempty"`
	Stream string `json:"stream,omitempty"` // stdout|stderr|status
	Line   string `json:"line,omitempty"`
	TS     int64  `json:"ts"`
	Error  string `json:"error,omitempty"`
	Result string `json:"result,omitempty"` // rootfs build id on success
}

// HealthCheck gates a rollout: a new release takes traffic only once healthy.
// The shape is Docker's, so an image's own HEALTHCHECK maps straight in.
type HealthCheck struct {
	Type string `json:"type,omitempty"` // "http" (default) | "cmd" | "none"
	Path string `json:"path,omitempty"`
	// Test is Docker's form: ["CMD-SHELL", "..."], ["CMD", argv...], ["NONE"].
	Test             []string `json:"test,omitempty"`
	IntervalSec      int      `json:"interval,omitempty"`
	TimeoutSec       int      `json:"timeout,omitempty"`
	GraceSec         int      `json:"grace,omitempty"`
	HealthyThreshold int      `json:"healthy_threshold,omitempty"`
}

type Service struct {
	// OrgID is the org that owns this object. Set when the caller is an admin
	// key, which is the only caller that sees objects across orgs; a
	// tenant-scoped key only ever sees its own.
	OrgID        string       `json:"org_id,omitempty"`
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	App          string       `json:"app,omitempty"`
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
	// Accepted for wire compatibility and not persisted -- a service row keeps
	// no knobs, so a policy set here goes nowhere and the deploy is where it
	// belongs. A patch for the same reason every other request's is.
	Knobs  *KnobsPatch  `json:"knobs,omitempty"`
	Health *HealthCheck `json:"health,omitempty"`
	// Domain is the subdomain label under the fleet's domain. Empty means the
	// service mints no route rows and is reachable over <name>.internal only.
	Domain       string `json:"domain,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	// Volume is create-only: a volume swap is a data migration, not a
	// configuration change, so the update route does not take it. Requires
	// replicas of at most one.
	Volume     string            `json:"volume,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	SecretEnv  map[string]string `json:"secret_env,omitempty"`
	Repo       string            `json:"repo,omitempty"`
	Branch     string            `json:"branch,omitempty"`
	Autodeploy bool              `json:"autodeploy,omitempty"`
}

type DeployRequest struct {
	Release string `json:"release,omitempty"`
	Build   string `json:"build,omitempty"`
	// Knobs is the lifecycle policy for the replicas this deploy creates,
	// merged onto what the previous release's replicas carry. A service row
	// keeps no knobs, so the deploy is where they travel.
	//
	// A patch, so raising one field does not zero the three the caller never
	// mentioned. See KnobsPatch.
	Knobs *KnobsPatch `json:"knobs,omitempty"`
}

// PromoteRequest turns a sandbox into a durable service. The machine's URL is
// unchanged by promotion; a custom domain is additive.
type PromoteRequest struct {
	CustomDomain string       `json:"custom_domain,omitempty"`
	Replicas     int          `json:"replicas,omitempty"`
	Health       *HealthCheck `json:"health,omitempty"`
}

// RedeployRequest boots a machine again from another image, in place: same
// row, same URL, same volume. How a volume-backed service takes a release.
// Sent by the rollout inside the fleet; there is no client method for it.
type RedeployRequest struct {
	Image   string `json:"image"`
	Release string `json:"release,omitempty"`
}

type Release struct {
	ID            string `json:"id"`
	ServiceID     string `json:"service_id"`
	RootfsBuildID string `json:"rootfs_build_id,omitempty"`
	MemBuildID    string `json:"mem_build_id,omitempty"`
	Healthy       bool   `json:"healthy"`
	CreatedAt     int64  `json:"created_at"`
}

// Volume is persistent, per-write-durable storage, attached to at most one
// machine and mounted by at most one host.
type Volume struct {
	// OrgID is the org that owns this object. Set when the caller is an admin
	// key, which is the only caller that sees objects across orgs; a
	// tenant-scoped key only ever sees its own.
	OrgID     string `json:"org_id,omitempty"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	SizeGiB   int    `json:"size_gib"`
	MachineID string `json:"machine_id,omitempty"`
	HostID    string `json:"host_id,omitempty"`
	MountPath string `json:"mount_path"`
	CreatedAt int64  `json:"created_at"`
}

// MachineVolume reports the volume drive a running machine actually has, read
// back out of Firecracker rather than repeated from what hostd meant to set.
// The difference between the two is a durability guarantee that fails
// silently.
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
	// Reflink reports whether the host's machine store can share extents.
	// Without it create and checkpoint are several times slower.
	Reflink bool `json:"reflink"`
	// HugePages reports whether guest memory on this host is backed by 2MiB
	// pages. Unlike Reflink this is not only a speed signal: the page size is
	// recorded in every snapshot and cannot be reinterpreted at restore, so a
	// host that disagrees with the fleet cannot restore the fleet's machines
	// at all.
	HugePages bool `json:"hugepages"`
	// StoreVersion is the sum of the local replica's version vector: how many
	// changes, from every host, this replica has applied. 0 on a single-box
	// SQLite host. Comparable across hosts, so two hosts far apart on this
	// number are a replication problem before they are anything else.
	StoreVersion int64 `json:"store_version"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type AddDomainRequest struct {
	ServiceID string `json:"service_id"`
	Hostname  string `json:"hostname"`
}

type DomainResponse struct {
	Hostname  string `json:"hostname"`
	ServiceID string `json:"service_id"`
	Verified  bool   `json:"verified"`
	// Target is what the customer's CNAME has to point at, returned on every
	// response including the failure.
	Target    string `json:"cname_target"`
	CreatedAt int64  `json:"created_at"`
}

// --- The data routes -----------------------------------------------------
//
// The service patch, the usage ledger and the compose plan. hostd serves all
// three, and the drift test checks every shape below against internal/api and
// internal/compose on each run.

// UpdateServiceRequest patches a service. Pointer fields so an absent value is
// distinguishable from a zero one; Env, SecretEnv and Replicas REPLACE what is
// stored and take effect at the next deploy. Knobs are refused here and travel
// on the deploy.
type UpdateServiceRequest struct {
	Replicas   *int              `json:"replicas,omitempty"`
	Health     *HealthCheck      `json:"health,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	SecretEnv  map[string]string `json:"secret_env,omitempty"`
	Repo       *string           `json:"repo,omitempty"`
	Branch     *string           `json:"branch,omitempty"`
	Autodeploy *bool             `json:"autodeploy,omitempty"`
}

type CreateAPIKeyRequest struct {
	OrgID  string   `json:"org_id"`
	Scopes []string `json:"scopes"`
}

type APIKeyResponse struct {
	// Key is the plaintext key, returned by the call that minted it and never
	// again.
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

type QuotaResponse struct {
	OrgID        string `json:"org_id"`
	MaxMachines  int    `json:"max_machines"`
	MaxVCPUs     int    `json:"max_vcpus"`
	MaxMemMiB    int    `json:"max_mem_mib"`
	MaxVolumeGiB int    `json:"max_volume_gib"`
	MaxBuilds    int    `json:"max_builds"`
	UpdatedAt    int64  `json:"updated_at"`
}

// QuotaExceededResponse is a 429 body. Scope is "host" when the ceiling is the
// host's rather than the org's, which is how builds are limited.
type QuotaExceededResponse struct {
	Error string `json:"error"`
	Quota string `json:"quota"`
	Limit int64  `json:"limit"`
	Used  int64  `json:"used"`
	Scope string `json:"scope,omitempty"`
}

type UsageTotals struct {
	MachineSeconds   int64 `json:"machine_seconds"`
	VCPUSeconds      int64 `json:"vcpu_seconds"`
	MiBSeconds       int64 `json:"mib_seconds"`
	VolumeGiBSeconds int64 `json:"volume_gib_seconds"`
}

type UsageResponse struct {
	HostID string                 `json:"host_id"`
	Since  int64                  `json:"since"`
	Until  int64                  `json:"until"`
	Orgs   map[string]UsageTotals `json:"orgs"`
}

// --- internal/compose, mirrored under a Compose prefix --------------------

type ComposeRequest struct {
	// Compose is the file's text, not a path.
	Compose string `json:"compose"`
	// Env is the interpolation environment for ${VAR}: the caller's .env file,
	// never the whole process environment.
	Env map[string]string `json:"env,omitempty"`
}

type ComposeBuild struct {
	Context    string `json:"context,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

type ComposeVolume struct {
	Name      string `json:"name"`
	SizeGiB   int    `json:"size_gib"`
	MountPath string `json:"mount_path"`
}

type ComposeStep struct {
	Name       string            `json:"name"`
	Build      *ComposeBuild     `json:"build,omitempty"`
	Dockerfile string            `json:"dockerfile,omitempty"`
	Cmd        string            `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	// SecretRefs maps an env key to a secret name; the value never appears.
	SecretRefs map[string]string `json:"secret_refs,omitempty"`
	Ports      []int             `json:"ports,omitempty"`
	Health     *HealthCheck      `json:"health,omitempty"`
	Volumes    []ComposeVolume   `json:"volumes,omitempty"`
	Replicas   int               `json:"replicas"`
	VCPUs      int               `json:"vcpus"`
	MemMiB     int               `json:"mem_mib"`
	DependsOn  []string          `json:"depends_on,omitempty"`
	// A patch: a step's knobs are whatever the compose file spelled out, and
	// they are spread straight onto a DeployRequest, so the fields the file
	// left out must stay absent rather than arrive as zeros.
	Knobs        *KnobsPatch `json:"knobs,omitempty"`
	Domain       string      `json:"domain,omitempty"`
	CustomDomain string      `json:"custom_domain,omitempty"`
	PreDeploy    string      `json:"pre_deploy,omitempty"`
}

type ComposePlan struct {
	App   string        `json:"app"`
	Steps []ComposeStep `json:"steps"`
}

type ComposeUnsupported struct {
	Service string `json:"service"`
	Key     string `json:"key"`
	Message string `json:"message"`
}

// wireTypes is every struct above, once. The drift test reflects over it, and
// fails when hostd carries a tagged struct nobody listed here -- so a new wire
// shape cannot land unmirrored.
//
// KnobsPatch is deliberately absent: it is not a hostd struct but the partial
// ENCODING of one, which hostd receives as a json.RawMessage. It is held to
// Knobs by TestKnobsPatchCoversEveryKnob instead.
var wireTypes = []any{
	Knobs{},
	Machine{},
	CreateMachineRequest{},
	ExecRequest{},
	ExecResponse{},
	CheckpointRequest{},
	Checkpoint{},
	BuildLogLine{},
	HealthCheck{},
	Service{},
	CreateServiceRequest{},
	DeployRequest{},
	PromoteRequest{},
	RedeployRequest{},
	Release{},
	Volume{},
	MachineVolume{},
	CreateVolumeRequest{},
	Host{},
	HealthResponse{},
	ErrorResponse{},
	AddDomainRequest{},
	DomainResponse{},
	UpdateServiceRequest{},
	CreateAPIKeyRequest{},
	APIKeyResponse{},
	RevokeResponse{},
	QuotaResponse{},
	QuotaExceededResponse{},
	UsageTotals{},
	UsageResponse{},
	ComposeRequest{},
	ComposeBuild{},
	ComposeVolume{},
	ComposeStep{},
	ComposePlan{},
	ComposeUnsupported{},
	ComposePlanError{},
}
