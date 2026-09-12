package pilots

// The wire contract, mirrored from apps/hostd/internal/api and
// apps/hostd/internal/compose.
//
// One struct per hostd struct, same name and same JSON tags; the structs from
// the compose package carry a Compose prefix, because Plan, Step and Request
// are far too generic to export unqualified. types_drift_test.go parses
// hostd's Go source on every run and fails naming the struct and the tag when
// the two sides disagree, in either direction.

import (
	"encoding/json"
	"strings"
)

// Knobs is the per-machine lifecycle policy. A sandbox and a production
// service are the same machine with different knobs.
type Knobs struct {
	AutoStop           string `json:"auto_stop"`            // off|suspend
	AutoStart          bool   `json:"auto_start"`           // wake on an inbound request
	MinMachinesRunning int    `json:"min_machines_running"` // 0 = scale to zero
	SoftLimit          int    `json:"soft_limit"`
	// HardLimit is the concurrency the machine queues at and then refuses.
	// soft_limit starts another replica; this one refuses. A request above it
	// waits briefly for room and is then answered 503 with Retry-After. Zero
	// is unlimited.
	HardLimit   int `json:"hard_limit"`
	IdleTimeout int `json:"idle_timeout"` // seconds of quiet before suspend, 1..3600
	// Schedules are the machine's cron jobs; null (nil here) means none. On a
	// deploy an absent key inherits the previous release's and an explicit
	// empty list clears them, which is why KnobsPatch carries a pointer to a
	// slice.
	//
	// No omitempty, matching hostd: it drops an EMPTY slice as readily as a
	// nil one, and the difference between the two is the difference between
	// "no crons, deliberately" and "inherit whatever was there".
	Schedules []Schedule `json:"schedules"`
}

// Schedule is one cron job: a five-field expression (UTC; or @hourly, @daily,
// @weekly, @monthly) and exactly one of a path the host GETs on the machine
// or a command it runs in it. A GET carries the X-Pilot-Cron header, which
// cannot arrive from outside the fleet.
type Schedule struct {
	Cron string `json:"cron"`
	Path string `json:"path,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
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
	AutoStop           *string `json:"auto_stop,omitempty"`            // off|suspend
	AutoStart          *bool   `json:"auto_start,omitempty"`           // wake on an inbound request
	MinMachinesRunning *int    `json:"min_machines_running,omitempty"` // 0 = scale to zero
	SoftLimit          *int    `json:"soft_limit,omitempty"`
	HardLimit          *int    `json:"hard_limit,omitempty"`
	IdleTimeout        *int    `json:"idle_timeout,omitempty"` // seconds of quiet before suspend, 1..3600
	// A pointer to a slice, not a slice: omitempty drops an empty slice, and
	// an empty list is the one way to clear inherited schedules on a deploy.
	// Ptr([]Schedule{}) sends "schedules": [].
	Schedules *[]Schedule `json:"schedules,omitempty"`
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
	// LastStart says how this machine last came up: "restore" of its memory
	// image, the "boot" a create with an image or a volume pays once (and
	// every redeploy), or "cold_boot", a restore DOWNGRADED because no host of
	// the image's CPU vendor was alive. A cold boot keeps the id, name, URL,
	// disk and volume, and loses everything that was in memory.
	LastStart   string `json:"last_start,omitempty"`
	LastStartAt int64  `json:"last_start_at,omitempty"`
	// Labels were attached at create, for finding the machine again.
	Labels map[string]string `json:"labels,omitempty"`
	// URLAuth is who may reach the URL: "public" (the default) or "org".
	URLAuth string `json:"url_auth,omitempty"`
	// Parent is the machine this one was FORKED from, and Checkpoint the
	// checkpoint it was restored from. Empty on a machine that was created
	// rather than forked.
	Parent     string `json:"parent,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	// Egress is the address this machine's OUTBOUND traffic leaves from, when
	// its host manages egress. Shared with the org's other machines on the
	// same host, and outliving every one of them.
	//
	// Empty means the machine leaves from the host's shared address, which is
	// what every machine did before egress addresses existed.
	Egress string `json:"egress,omitempty"`
}

// EgressResponse is every address an org's outbound traffic can leave from,
// one per host that manages egress. GET /v1/egress.
//
// A set rather than one address, because the address is derived from the
// HOST's prefix: an org running machines on three hosts leaves from three
// addresses. It changes when a host joins or leaves the fleet and at no other
// time -- not when the org's machines are created, destroyed, resized, rolled
// or moved -- which is what makes it safe to put in somebody else's firewall.
type EgressResponse struct {
	OrgID     string          `json:"org_id"`
	Addresses []EgressAddress `json:"addresses"`
}

// EgressAddress is one host's answer. There is no IPv4 counterpart and there
// will not be one: a v4 address is purchased and scarce, and a bare-metal host
// has one, so v4 stays a shared masquerade.
type EgressAddress struct {
	HostID    string `json:"host_id"`
	IPv6      string `json:"ipv6"`
	Interface string `json:"interface,omitempty"`
}

// URL auth modes.
const (
	URLAuthPublic = "public"
	URLAuthOrg    = "org"
)

// Size is how big a machine is: the two dimensions that are priced, named
// together wherever a service carries a size rather than a single machine.
//
// Zero on a dimension means "leave it as it is" on a request, and means the
// default on a reply -- never a machine with no memory.
type Size struct {
	VCPUs  int `json:"vcpus"`
	MemMiB int `json:"mem_mib"`
}

// ResizeMachineRequest is POST /v1/machines/{id}/resize. Either field may be
// omitted to leave that dimension alone.
type ResizeMachineRequest struct {
	VCPUs  int `json:"vcpus,omitempty"`
	MemMiB int `json:"mem_mib,omitempty"`
}

// UpdateMachineRequest is PATCH /v1/machines/{id}: who may reach the URL.
type UpdateMachineRequest struct {
	URLAuth *string `json:"url_auth,omitempty"`
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
	Labels    map[string]string `json:"labels,omitempty"`
	URLAuth   string            `json:"url_auth,omitempty"` // public|org; default public
}

// Process is one of the named processes a machine runs.
//
// A machine runs a SET of them: an image's own command is the process "app",
// and a compose file or a runtime registration can add more. The names are
// what make it possible to restart a dev server without taking down the
// database beside it.
type Process struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd"`
	// State is "running" or "stopped".
	State string `json:"state"`
	// PID inside the guest. Zero when the process is stopped.
	PID int `json:"pid,omitempty"`
	// Restarts counts how often the supervisor brought it back after an exit
	// nobody asked for. Climbing steadily is a crash loop.
	Restarts int `json:"restarts"`
	// Needs names processes that must start before this one.
	Needs []string `json:"needs,omitempty"`
	// Port reports the one process that owns the machine's app port.
	Port bool `json:"port,omitempty"`
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
	// Code is the stable code on a terminal failure line, build_failed.
	Code string `json:"code,omitempty"`
	// Release is the deployment cut from this image, on the last line of a
	// build whose request named a service to deploy. Its presence is what
	// says the release exists: Result says only that the image does.
	Release string `json:"release,omitempty"`
	// Next is the reader's next step on a terminal failure line. A deploy
	// refused after the image was built -- a health gate that never passed,
	// above all -- reaches the reader through the log, so it carries the same
	// next the deploy route would have answered with.
	Next string `json:"next,omitempty"`
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
	OrgID string `json:"org_id,omitempty"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	App   string `json:"app,omitempty"`
	// DependsOn names the sibling services in this app whose <name>.internal
	// address this service's environment references. Derived by hostd on
	// every read from both halves of the environment and stored nowhere, so
	// it reflects what the service is configured to dial right now.
	DependsOn []string `json:"depends_on,omitempty"`
	Replicas  int      `json:"replicas"`
	// Size is how big each replica is. Always spelled out, even for a service
	// that has never been scaled, so a reader never has to know what the
	// defaults were on the day the service was made.
	Size         Size         `json:"size"`
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
	// Labels were attached at create, or copied from the machine promote made it from.
	Labels  map[string]string `json:"labels,omitempty"`
	URLAuth string            `json:"url_auth,omitempty"`
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
	// Size is how big each replica will be. Omitted means the defaults, which
	// is what every service was before a service had a size.
	Size *Size `json:"size,omitempty"`
	// Domain is the subdomain label under the fleet's domain. Empty means one
	// is minted from the name: the name itself when it is free, else the name
	// and a four-character suffix. Set it to ask for an exact label, which is
	// taken literally or refused, never adjusted.
	Domain string `json:"domain,omitempty"`
	// Private mints no address at all. The service is reachable by peers over
	// <name>.internal, and its replicas keep their own machine URLs the way
	// every machine does. Create-only: an address, once minted, is permanent.
	Private      bool   `json:"private,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	// Volume is create-only: a volume swap is a data migration, not a
	// configuration change, so the update route does not take it. Requires
	// replicas of at most one.
	Volume     string            `json:"volume,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	SecretEnv  map[string]string `json:"secret_env,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	URLAuth    string            `json:"url_auth,omitempty"` // public|org; default public
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
	// Size sets how big the replicas this deploy creates are.
	//
	// It rides on the deploy rather than being sent as a separate patch
	// beforehand: a patch carrying a size runs a rollout of its own, so a
	// compose file that changed both its image and its size would roll the
	// service twice to arrive where one rollout could have put it.
	Size *Size `json:"size,omitempty"`
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
	// CPUVendor is which pool this host restores memory images from, the raw
	// /proc/cpuinfo vendor_id. Empty until the host publishes it.
	CPUVendor string `json:"cpu_vendor,omitempty"`
	// MemReclaimableMiB is memory held by RUNNING machines this host would
	// suspend if it needed the room. Placement counts it as available, so a
	// host whose free memory looks small can still take a create. Suspended
	// machines are not counted: their memory is already in mem_free_mib.
	MemReclaimableMiB int `json:"mem_reclaimable_mib"`
	// VCPUsRunning is the vCPUs this host's machines are configured with.
	// Oversubscription is normal, because vCPUs are timeshared, so this is a
	// load signal rather than a limit.
	VCPUsRunning int `json:"vcpus_running"`
	// Draining says an operator is moving this host's machines off it. Every
	// ranker skips a draining host.
	Draining bool `json:"draining,omitempty"`
	// BuildsCached is how many builds this host holds on local disk.
	BuildsCached int `json:"builds_cached,omitempty"`
}

// ForkRequest asks for N new machines from one machine's or checkpoint's exact
// state: the source's processes already running, its memory already warm.
type ForkRequest struct {
	// Name is the first fork's name; the rest take a suffix. Empty mints one.
	Name string `json:"name,omitempty"`
	// Count is how many, default 1, capped at 100.
	Count int `json:"count,omitempty"`
	// Volume forks the source's volume too. A source WITH a volume and this
	// unset is refused rather than forked without it.
	Volume bool `json:"volume,omitempty"`
}

// ForkResponse is one entry per requested fork, in order.
//
// Per-fork rather than one status for the request, because forks are
// independent: nine that came up are worth having when the tenth did not.
type ForkResponse struct {
	Forks []ForkEntry `json:"forks"`
}

// ForkEntry is one fork: the machine, or why it did not happen.
type ForkEntry struct {
	Machine *Machine `json:"machine,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// SnapshotResponse is one point-in-time copy of a volume.
//
// Snapshot is the stamp that names it, `20260912T101500Z`. It sorts lexically
// in time order, so a list needs no separate ordering field.
type SnapshotResponse struct {
	VolumeID string `json:"volume_id"`
	Snapshot string `json:"snapshot"`
}

// ComposeRecipe is the compose fragment for one database, with the durability
// decision made and explained.
//
// Fetched rather than built by the client: two copies of a recipe is two places
// for it to drift from what the planner will accept. The PASSWORD is generated
// on the client and never crosses the wire -- SecretNames says what to make,
// and URLTemplate carries PASSWORD where it goes.
type ComposeRecipe struct {
	Engine string `json:"engine"`
	// Mode is "wal-archive" or "durable-volume" for Postgres, and
	// "durable-volume" for every other engine.
	Mode string `json:"mode"`
	// Service is the compose service block, ready to splice into a file.
	Service map[string]any `json:"service"`
	// Companions are further compose services the recipe declares, by name.
	// Splice each one in beside Service. They build from the same context, so
	// the planner folds them into the database's own machine as extra
	// processes rather than standing up a second one.
	Companions map[string]map[string]any `json:"companions,omitempty"`
	// Volumes are the named volumes it declares.
	Volumes map[string]struct{} `json:"volumes"`
	// Files are extra files the fragment needs, by path relative to the
	// project root. Anything ending .sh is written executable.
	Files map[string]string `json:"files,omitempty"`
	// SecretNames are the secrets to generate and store locally.
	SecretNames []string `json:"secret_names"`
	// ConnVar is the environment variable an application reads, and
	// URLTemplate its value with PASSWORD standing in for the secret.
	ConnVar     string `json:"conn_var"`
	URLTemplate string `json:"url_template"`
	// DirectVar and DirectTemplate are the second connection string, past the
	// pooler, present only when there is a pooler. Transaction pooling is not a
	// superset of a direct connection -- it costs LISTEN/NOTIFY, session
	// advisory locks, temporary tables and any SET that outlives a transaction
	// -- so the address a migration must use is named rather than guessed.
	DirectVar      string `json:"direct_var,omitempty"`
	DirectTemplate string `json:"direct_template,omitempty"`
	// Statement is what this mode costs and guarantees, in one line. Print it:
	// a durability decision the operator did not read is one they did not make.
	Statement string `json:"statement"`
}

// URLFor is the connection string with the generated password in it.
//
// The template travels with PASSWORD where the secret goes, so the value is
// made on the caller's machine and the fleet never sees it.
func (r *ComposeRecipe) URLFor(password string) string {
	return strings.Replace(r.URLTemplate, "PASSWORD", password, 1)
}

// DirectURLFor is the connection string that goes PAST the pooler. Empty when
// the recipe has no pooler.
func (r *ComposeRecipe) DirectURLFor(password string) string {
	if r.DirectTemplate == "" {
		return ""
	}
	return strings.Replace(r.DirectTemplate, "PASSWORD", password, 1)
}

// VolumePolicy is how often a volume is snapshotted and how much is kept.
//
// Two retention numbers rather than one, because they answer different
// questions: how far back at a day's resolution, and how far back at all.
//
// An empty Cron means no schedule. Retention of zero and zero keeps
// EVERYTHING, never nothing: an unset policy read as "keep none" would delete
// a volume's whole history the first time the loop ran.
type VolumePolicy struct {
	Cron       string `json:"cron,omitempty"`
	KeepDaily  int    `json:"keep_daily,omitempty"`
	KeepWeekly int    `json:"keep_weekly,omitempty"`
}

// ForkVolumeRequest names the new volume a fork creates. Empty mints one from
// the source's name and the snapshot's stamp.
type ForkVolumeRequest struct {
	Name string `json:"name,omitempty"`
}

// SnapshotListResponse is every snapshot of a volume, newest first.
type SnapshotListResponse struct {
	VolumeID  string   `json:"volume_id"`
	Snapshots []string `json:"snapshots"`
}

// DrainReport is what draining a host did. POST /v1/hosts/{id}/drain.
//
// The machines in Moved are on other hosts now, with the same ids, names and
// URLs they had: that is what makes a drain invisible to the people using them.
// Left are the ones no host would take, each with its reason -- a fleet-capacity
// problem, reported rather than retried for ever.
type DrainReport struct {
	Moved  []string          `json:"moved"`
	Left   []string          `json:"left,omitempty"`
	Errors map[string]string `json:"errors,omitempty"`
	// Draining stays true after a drain that left something behind, so the
	// host goes on refusing new machines until an operator says otherwise.
	Draining bool  `json:"draining"`
	Started  int64 `json:"started"`
}

// TakeRequest is one host telling another to take a machine it has offered.
// Internal: it travels over the mesh, and the offer row is what authorises the
// move. No client sends this.
type TakeRequest struct {
	HandoffID string `json:"handoff_id"`
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
	// CPUVendor is this host's vendor pool, the raw /proc/cpuinfo vendor_id.
	// A memory image never restores across the Intel/AMD boundary, so this is
	// what says which of the fleet's snapshots this host can load.
	CPUVendor string `json:"cpu_vendor"`
	// CPUVendorForced is true only when a fault flag is making this host lie
	// about its CPU, which is how the fleet gate reaches the cold-boot tier.
	CPUVendorForced bool `json:"cpu_vendor_forced,omitempty"`
	// StoreVersions is the same number broken out per actor: how far this
	// replica has applied each host's changes, keyed by site id in hex. The
	// sum answers "are we far apart"; this answers "on whose rows". Empty on
	// a single-box SQLite host.
	StoreVersions map[string]int64 `json:"store_versions,omitempty"`
	// ReplicationComplete is false while this host is still catching up with
	// the fleet. Such a host serves its own machines normally and claims none
	// of anybody else's, so it is healthy, not broken. Stuck false for more
	// than a few seconds is a replication problem.
	ReplicationComplete bool `json:"replication_complete"`
}

// WhoamiResponse is what the caller's key resolves to on the host that
// answered. OrgID is empty for a key that belongs to no org, which is the
// bootstrap admin key's case.
type WhoamiResponse struct {
	OrgID  string   `json:"org_id"`
	Scopes []string `json:"scopes"`
	HostID string   `json:"host_id"`
}

type ErrorResponse struct {
	Error string `json:"error"`
	// Code is a stable snake_case noun to branch on. See
	// apps/hostd/internal/api/errors.go for the closed list.
	Code string `json:"code,omitempty"`
	// Next is the one thing to do about it, naming the command or the call.
	Next string `json:"next,omitempty"`
	// Details is typed per code: HealthGateDetails, ComposeUnknownDetails.
	Details json.RawMessage `json:"details,omitempty"`
}

// HealthGateDetails is the 422 health_gate_failed body's details: why a
// release was refused. It carries no address, because the probe target is the
// host's own view of the replica and is not reachable from where this is read.
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
	Replicas *int `json:"replicas,omitempty"`
	// Size changes how big every replica is. Zero on a dimension leaves that
	// dimension alone.
	//
	// Applying it replaces the replicas one at a time, at the same release,
	// and drops no request. A volume-backed service has a held window instead,
	// because a volume has one writer and the replacement cannot mount it
	// until the old machine has let go.
	Size       *Size             `json:"size,omitempty"`
	Health     *HealthCheck      `json:"health,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	SecretEnv  map[string]string `json:"secret_env,omitempty"`
	Repo       *string           `json:"repo,omitempty"`
	Branch     *string           `json:"branch,omitempty"`
	Autodeploy *bool             `json:"autodeploy,omitempty"`
	// Domain gives an address to a service that has none, which is the only
	// way one created before addresses were minted, or one created private,
	// can get one. Accepted exactly once: a service that already has an
	// address is a 409 and an empty string a 400, because URLs are permanent.
	Domain *string `json:"domain,omitempty"`
	// URLAuth changes who may reach the address: "public" or "org".
	URLAuth *string `json:"url_auth,omitempty"`
}

type CreateAPIKeyRequest struct {
	OrgID  string   `json:"org_id"`
	Scopes []string `json:"scopes"`
	// The three RESTRICTIONS, all optional and all enforced by the fleet.
	// NamePrefix is what every machine and service the key names must start
	// with; MaxMachines caps how many of them may exist at once; ExpiresAt is
	// unix seconds after which the key authenticates nothing. Absent means
	// unrestricted, which is what an operator's own key is.
	//
	// Write-once with the key: a restriction that could be widened later
	// would not be one, so a key is minted again rather than edited.
	NamePrefix  string `json:"name_prefix,omitempty"`
	MaxMachines int    `json:"max_machines,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
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
	// The restrictions this key carries, echoed on the mint and on every
	// listing. Absent means unrestricted.
	NamePrefix  string `json:"name_prefix,omitempty"`
	MaxMachines int    `json:"max_machines,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
}

type RevokeResponse struct {
	Hash      string `json:"hash"`
	RevokedAt int64  `json:"revoked_at"`
}

// ConnectRepoRequest ties a repository to an org, which is what lets that
// org's own keys name it in a {repo, ref} build or plan. Admin-scoped: the
// proof that an org controls a repository is held at GitHub, so the connection
// is asserted by a party that can prove it and recorded once.
//
// The org is not in the body. It comes from the key, or from ?org= on an admin
// key, as it does on every other create.
type ConnectRepoRequest struct {
	Repo string `json:"repo"`
}

// RepoLinkResponse is one connection between an org and a repository.
type RepoLinkResponse struct {
	Repo        string `json:"repo"`
	OrgID       string `json:"org_id"`
	ConnectedAt int64  `json:"connected_at"`
}

type RepoLinkListResponse struct {
	Repos []RepoLinkResponse `json:"repos"`
}

type QuotaResponse struct {
	OrgID        string `json:"org_id"`
	MaxMachines  int    `json:"max_machines"`
	MaxVCPUs     int    `json:"max_vcpus"`
	MaxMemMiB    int    `json:"max_mem_mib"`
	MaxVolumeGiB int    `json:"max_volume_gib"`
	MaxBuilds    int    `json:"max_builds"`
	// MaxSnapshotGiB is how much object storage this org's checkpoints may
	// hold. Zero on a PUT means the default rather than none, so a client
	// written against the older body shape does not freeze an org's
	// checkpoints by omitting it.
	MaxSnapshotGiB int   `json:"max_snapshot_gib"`
	UpdatedAt      int64 `json:"updated_at"`
}

// QuotaExceededResponse is a 429 body. Scope is "host" when the ceiling is the
// host's rather than the org's, which is how builds are limited.
type QuotaExceededResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Next  string `json:"next"`
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
	// SnapshotGiBSeconds is what this org's checkpoints held in object
	// storage, accrued in EVERY machine state: the bytes are there whatever
	// the guest is doing, which is why a stopped machine is not free.
	SnapshotGiBSeconds int64 `json:"snapshot_gib_seconds"`
}

type UsageResponse struct {
	HostID string                 `json:"host_id"`
	Since  int64                  `json:"since"`
	Until  int64                  `json:"until"`
	Orgs   map[string]UsageTotals `json:"orgs"`
	// Machines is the same accrual per machine, keyed by org and then by
	// machine id. Present only for a ByMachine call, because it is the larger
	// answer and most callers want the invoice line rather than its
	// derivation.
	Machines map[string]map[string]UsageTotals `json:"machines,omitempty"`
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

// What a compose step gets when its file says nothing about size. Mirrored
// from internal/compose so a caller can tell "the file asked for this" from
// "the planner filled in the default", which is the difference between
// changing a service's size and leaving it alone.
const (
	DefaultVCPUs  = 1
	DefaultMemMiB = 512
)

type ComposeStep struct {
	Name       string        `json:"name"`
	Build      *ComposeBuild `json:"build,omitempty"`
	Dockerfile string        `json:"dockerfile,omitempty"`
	// DockerfileAppend is what the compose file overrode -- command:,
	// entrypoint:, working_dir:, user: -- rendered as Dockerfile instructions
	// to append to the build context's own Dockerfile before uploading it.
	DockerfileAppend string            `json:"dockerfile_append,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
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
	Knobs  *KnobsPatch `json:"knobs,omitempty"`
	Domain string      `json:"domain,omitempty"`
	// Private asks for no address at all. A service without it is given one
	// from its name, so this is how a database says it has nothing to serve.
	Private bool `json:"private,omitempty"`
	// Labels are attached to the service at create, write-once. The database
	// recipes set `pilot.engine`, which is how `pilot metrics` and the
	// dashboard know a service is a database and which one.
	Labels map[string]string `json:"labels,omitempty"`
	// SnapshotPolicy is the volume's schedule and retention. Nil leaves
	// whatever is set, so a redeploy does not reset a schedule somebody tuned.
	SnapshotPolicy *VolumePolicy `json:"snapshot_policy,omitempty"`
	CustomDomain   string        `json:"custom_domain,omitempty"`
	PreDeploy      string        `json:"pre_deploy,omitempty"`
	// Processes is filled when SEVERAL compose services share one build
	// context and therefore run as one machine with one process each. Empty is
	// the ordinary case: one service, one machine, one process named app.
	Processes []ComposeProcess `json:"processes,omitempty"`
}

// ComposeProcess is one named command inside a machine that runs several.
type ComposeProcess struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd,omitempty"`
	// Needs orders the start within the machine: everything named here starts
	// before this process does.
	Needs []string `json:"needs,omitempty"`
	// Port marks the one process that owns the machine's published port.
	Port bool `json:"port,omitempty"`
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

// ComposePlanResponse is POST /v1/plan's 200 body: the plan, and how each
// step was decided.
type ComposePlanResponse struct {
	Plan     ComposePlan       `json:"plan"`
	Detected []ComposeDetected `json:"detected"`
}

// ComposeDetected says where one step came from. Source is "compose",
// "dockerfile" or "recipe"; Framework and Notes are set for a recipe only.
type ComposeDetected struct {
	Service   string       `json:"service"`
	Source    string       `json:"source"`
	Framework string       `json:"framework,omitempty"`
	Dir       string       `json:"dir"`
	Port      int          `json:"port"`
	Health    *HealthCheck `json:"health,omitempty"`
	Notes     []string     `json:"notes,omitempty"`
}

// ComposeUnknownDetails is the 400 unknown_framework's details: everything
// needed to write the Dockerfile by hand, so the refusal is a starting point
// and not a dead end.
type ComposeUnknownDetails struct {
	Dir        string            `json:"dir"`
	LookedFor  []string          `json:"looked_for"`
	Listing    []string          `json:"listing"`
	Manifests  map[string]string `json:"manifests,omitempty"`
	Workspaces []string          `json:"workspaces,omitempty"`
	// Rules are the two lines every Dockerfile must obey.
	Rules []string `json:"rules"`
}

// RepoRef is the body POST /v1/plan and POST /v1/builds accept in place of a
// tar: a repository the fleet's GitHub App is installed on, at a ref. The host
// fetches the bytes itself, through the path a push takes, so no client has to
// hold them.
type RepoRef struct {
	Repo string `json:"repo"` // owner/name
	Ref  string `json:"ref"`  // branch, tag or sha
}

// wireTypes is every struct above, once. The drift test reflects over it, and
// fails when hostd carries a tagged struct nobody listed here -- so a new wire
// shape cannot land unmirrored.
//
// KnobsPatch is deliberately absent: it is not a hostd struct but the partial
// ENCODING of one, which hostd receives as a json.RawMessage. It is held to
// Knobs by TestKnobsPatchCoversEveryKnob instead.
var wireTypes = []any{
	UpdateMachineRequest{},
	ResizeMachineRequest{},
	Size{},
	EgressResponse{},
	EgressAddress{},
	ForkRequest{},
	ForkResponse{},
	ForkEntry{},
	SnapshotResponse{},
	VolumePolicy{},
	ComposeRecipe{},
	ForkVolumeRequest{},
	SnapshotListResponse{},
	DrainReport{},
	TakeRequest{},
	Knobs{},
	Schedule{},
	Machine{},
	CreateMachineRequest{},
	ExecRequest{},
	ExecResponse{},
	CheckpointRequest{},
	Checkpoint{},
	BuildLogLine{},
	HealthCheck{},
	Service{},
	RepoRef{},
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
	WhoamiResponse{},
	ErrorResponse{},
	HealthGateDetails{},
	HealthLast{},
	AddDomainRequest{},
	DomainResponse{},
	UpdateServiceRequest{},
	CreateAPIKeyRequest{},
	APIKeyResponse{},
	RevokeResponse{},
	ConnectRepoRequest{},
	RepoLinkResponse{},
	RepoLinkListResponse{},
	QuotaResponse{},
	QuotaExceededResponse{},
	UsageTotals{},
	UsageResponse{},
	ComposeRequest{},
	ComposeBuild{},
	ComposeVolume{},
	ComposeStep{},
	ComposeProcess{},
	ComposePlan{},
	ComposeUnsupported{},
	ComposePlanError{},
	ComposePlanResponse{},
	ComposeDetected{},
	ComposeUnknownDetails{},
}
