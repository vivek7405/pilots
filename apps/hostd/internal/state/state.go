// Package state is hostd's view of cluster state.
//
// The Store interface is deliberately narrow and driver-agnostic: phase 2 runs
// it over a local SQLite file, and phase 4 swaps in a Corrosion-backed driver
// that speaks the SAME schema (schema.sql is loaded verbatim by Corrosion).
// Nothing above this package may assume which driver is underneath.
//
// Two rules bind every caller (see ARCHITECTURE.md):
//
//   - Single-writer: a host writes ONLY rows describing its own machines. The
//     sanctioned exceptions are deterministic-owner operations -- name
//     allocation and self-heal claims of a provably dead host's machines --
//     and the tables whose rows describe no machine: tenancy,
//     api_key_revocations and repo_links, which are write-once, and api_keys
//     and org_quotas, written by any host serving an admin-scoped request. A
//     row is safe for "any host" only when it is written once or has one
//     logical writer.
//   - Reads are local and must never block on another host. Routing and wake
//     depend on this.
package state

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: hostd builds with CGO_ENABLED=0
)

//go:embed schema.sql
var Schema string

// ErrNotFound is returned by the Get* methods when no row matches.
var ErrNotFound = errors.New("state: not found")

// Machine is one Firecracker microVM's row. It is the platform's only
// primitive: a sandbox and a production service differ by Knobs, not by type.
type Machine struct {
	ID             string
	Name           string
	HostID         string
	State          string // creating|running|suspended|stopped|error
	KindKnobs      string // json
	ImageRef       string
	VCPUs          int
	MemMiB         int
	Domain         string
	CustomDomain   string
	AppPort        int
	AgentPort      int
	AgentTokenHash string
	MemBuildID     string
	RootfsBuildID  string
	// The template this machine was built from. Its images are diffs against
	// these, and restoring against any other template is corruption.
	TemplateMemBuildID    string
	TemplateRootfsBuildID string
	VolumeID              string
	ServiceID             string
	ReleaseID             string
	// App groups machines that may find and reach each other. Grouping only:
	// there is no apps table, because an app is a property of the client's
	// compose file rather than a fleet object.
	App string
	// Slot is the netns index this machine holds on HostID, and the low 16
	// bits of its mesh address. It changes when the machine is rescued onto
	// another host, which is why .internal answers cannot be cached.
	Slot         int
	LastActivity int64
	UpdatedAt    int64
}

// Host is one member of the fleet. Every host heartbeats its own row; a row
// whose last_seen is stale is what triggers self-heal on the survivors.
type Host struct {
	ID string
	// WGAddr is DERIVED from WGPubKey, never assigned. Anything that hands out
	// mesh addresses is an allocator, and an allocator is a control plane.
	WGAddr     string
	WGPubKey   string
	PublicIP   string
	CPUFree    int
	MemFreeMiB int
	LastSeen   int64
	// Vendor is read from host_cpu, never from a column of hosts: hosts carries
	// rows, and a column add there is the backfill hard rule 6 forbids. Empty
	// until the host_cpu row arrives, which ranks as "in no pool".
	Vendor string
}

// Checkpoint is a named, restorable point in a machine's life. A checkpoint
// and a release are the same artifact; that equivalence is what makes promote
// and rollback the same operation.
type Checkpoint struct {
	ID            string
	MachineID     string
	Seq           int
	Comment       string
	SourceID      string
	MemBuildID    string
	RootfsBuildID string
	Durable       bool
	CreatedAt     int64

	// ResumeGapMS is how long the guest was frozen for this checkpoint. Not
	// persisted: it describes one event, not the checkpoint's contents.
	ResumeGapMS int64 `json:"-"`
}

// A host writes ONLY rows describing its own machines. Corrosion accepts a
// violating write and merges it -- no error, no conflict, no log line -- and
// the damage surfaces later as a row two hosts both believe they own. So the
// rule is enforced in the driver, and the three sanctioned exceptions each
// need an explicit option, so that none of them can happen by accident.

// WriteOption authorises a write that would otherwise breach single-writer.
type WriteOption func(*WriteAuth)

// WriteAuth carries what a driver needs to check an exception.
type WriteAuth struct {
	// NameAllocation: this host is the deterministic owner of the name,
	// hash(name) mod live_hosts == my rank.
	NameAllocation bool
	// DeadOwnerClaim names the host being claimed from. The driver re-checks
	// that host's last_seen immediately before writing, because the claim is
	// only legitimate while the owner is still gone.
	DeadOwnerClaim string
	// HandoffID names a machine_handoffs row: one live host offering a
	// machine to another on a planned drain. The driver reads the row and
	// checks it; the id alone authorises nothing.
	HandoffID string
	// APIKeyWrite: an admin-scoped request on any host writing api_keys, one
	// of the tables whose rows describe no machine. The dashboard used to be
	// the only writer; it is a guest on the platform now and reaches the API
	// like any other client.
	APIKeyWrite bool
}

// WithNameAllocation authorises the write that reserves a machine name.
func WithNameAllocation() WriteOption {
	return func(a *WriteAuth) { a.NameAllocation = true }
}

// WithDeadOwnerClaim authorises taking a machine from a host that has stopped
// heartbeating. The driver verifies that independently; passing the option
// does not assert it.
func WithDeadOwnerClaim(ownerID string) WriteOption {
	return func(a *WriteAuth) { a.DeadOwnerClaim = ownerID }
}

// WithHandoff authorises taking a machine a LIVE host has offered.
//
// The third sanctioned exception to single-writer, and the only one where a
// live host's machine changes owner. The driver verifies the offer
// independently -- who offered it, to whom, that it is the newest offer for
// that machine, and that the machine is not running -- so passing this option
// asserts nothing by itself.
//
// Named by the handoff row's id rather than by the host, unlike
// WithDeadOwnerClaim, because the offer is the authority here: a host that
// merely says "I am taking this from host-b" is asserting something it cannot
// know, while a host presenting an offer row is quoting something host-b wrote.
func WithHandoff(handoffID string) WriteOption {
	return func(a *WriteAuth) { a.HandoffID = handoffID }
}

// WithAPIKeyWrite authorises an admin-scoped request's api_keys write.
func WithAPIKeyWrite() WriteOption {
	return func(a *WriteAuth) { a.APIKeyWrite = true }
}

// ResolveAuth collects options into one authorisation.
func ResolveAuth(opts []WriteOption) WriteAuth {
	var a WriteAuth
	for _, opt := range opts {
		opt(&a)
	}
	return a
}

// ErrNotOwner reports a write to a machine this host does not own.
var ErrNotOwner = errors.New("state: this host does not own that machine")

// StateDestroyed is the tombstone. Reads filter it; a reaper collects the rows
// after a retention window.
const StateDestroyed = "destroyed"

// StateRunning is the one other state this package names, because the handoff
// claim refuses a machine that is still up and the check belongs in the SQL
// rather than in a string the caller passes down.
const StateRunning = "running"

// Template is the golden image machines are created from, shared by the whole
// fleet. See the schema for why it cannot be per host.
type Template struct {
	ID            string
	MemBuildID    string
	RootfsBuildID string
	SnapKey       string
	CreatedAt     int64
}

// GoldenTemplateFor is the id of a vendor pool's template row. One row per
// pool, because the template's memory image is a Firecracker snapshot and a
// snapshot never restores across the Intel/AMD boundary.
func GoldenTemplateFor(vendor string) string { return "golden-" + vendor }

// BuilderTemplateFor is the id of a vendor pool's BUILDER template row: the
// golden guest plus a BuildKit daemon, which is what a per-org builder machine
// is created from. Per pool for the same reason as the golden row, and it is
// not optional here: a builder is idle-suspended between builds, so it has a
// memory image, and a memory image never restores across the vendor boundary.
func BuilderTemplateFor(vendor string) string { return "builder-" + vendor }

// HostCPU is what a host says about its own CPU. Written by the host itself,
// before its first heartbeat, so a peer never ranks it into a pool it is not in.
type HostCPU struct {
	HostID      string
	Vendor      string
	CPUTemplate string
	UpdatedAt   int64
}

// HostCapacity is what a host can still hold, written by that host on its
// heartbeat and read by every host that ranks a create.
//
// MemReclaimableMiB is memory held by RUNNING machines this host would suspend
// anyway, were the idle timer to fire now. NOT suspended machines: suspend
// kills the Firecracker process, so a suspended machine already holds no
// memory, and counting it would double-count free memory and admit creates
// that then fail to boot.
type HostCapacity struct {
	HostID            string
	MemFreeMiB        int
	MemReclaimableMiB int
	CPUCount          int
	VCPUsRunning      int
	// Draining is set by an operator. A draining host is skipped by every
	// ranker, which is what lets a drain converge instead of racing the
	// placer for the machines it is trying to move off.
	Draining  bool
	UpdatedAt int64
}

// Headroom is the memory a placement may use: what is free now plus what this
// host would free anyway.
func (c HostCapacity) Headroom() int { return c.MemFreeMiB + c.MemReclaimableMiB }

// HostBuilds is which builds a host already has on local disk, so a create can
// prefer a host that need not download them.
type HostBuilds struct {
	HostID    string
	Builds    []string
	UpdatedAt int64
}

// MaxCachedBuildsPublished bounds how many build ids a host advertises.
//
// Not cosmetic. A cr-sqlite row is gossiped in full on every change, so an
// unbounded value here starves the apply loop for every other row on the
// fleet -- the C5 landmine. A few hundred ids is far more than a placement
// decision needs and keeps the row small.
const MaxCachedBuildsPublished = 256

// The kinds of object a MachineCPU row can describe. Keyed like tenancy,
// because a release's and a checkpoint's memory images are as vendor-locked as
// a machine's and a mixed fleet could otherwise neither deploy nor roll back.
const (
	KindMachine    = "machine"
	KindRelease    = "release"
	KindCheckpoint = "checkpoint"
)

// How a machine last came up. StartColdBoot is a restore that was DOWNGRADED
// because no host of the memory image's vendor was alive: the machine keeps its
// id, name, URL, volume and disk, and loses everything that was in memory.
const (
	StartRestore  = "restore"
	StartBoot     = "boot"
	StartColdBoot = "cold_boot"
)

// MachineCPU records which CPU vendor photographed a memory image, and -- for a
// machine -- how that machine last started.
// Labels is what a caller attached to a machine or a service at create.
// Kind says which, because the ids share one table.
type Labels struct {
	ID        string
	Kind      string // machine|service
	Labels    map[string]string
	UpdatedAt int64
}

// URLAuth is who may reach a machine's or a service's URL.
type URLAuth struct {
	ID        string
	Kind      string // machine|service
	Mode      string // public|org
	UpdatedAt int64
}

// BrokerGrant is what a machine may ask its host's broker for.
//
// A machine holds no API key; it asks, and this says what the answer may be.
// Absent, or present with both Scopes and Sealed empty, means NO. Deny by
// default is the whole shape: a machine nobody granted anything to reaches
// nothing, and there is no state in the guest that could change that.
//
// Sealed is a seal.Seal of a json name-to-value map, never plaintext, because
// this row gossips to every host like every other one.
type BrokerGrant struct {
	ID        string
	Kind      string // machine|service
	OrgID     string
	Scopes    []string
	Sealed    string
	UpdatedAt int64
}

// DefaultServiceVCPUs and DefaultServiceMemMiB are what a service's replicas
// are, and always were, when nothing says otherwise. An absent service_sizes
// row reads as these rather than as zero, so every service that predates the
// table keeps the size it has been running at.
const (
	DefaultServiceVCPUs  = 1
	DefaultServiceMemMiB = 512
)

// HostEgress is the IPv6 block one host hands per-org egress addresses out of.
//
// Read by whichever host is answering a request about a machine, which is why
// it is replicated rather than kept in each host's own configuration: the
// answering host is usually not the host the machine is on.
type HostEgress struct {
	HostID    string
	Prefix6   string
	Interface string
	UpdatedAt int64
}

// ServiceSize is how big a service's replicas are.
//
// ImageVCPUs and ImageMemMiB are the size the current release's memory image
// was photographed at, which can differ from the size a replica is created
// with: a resize changes the size first and re-photographs afterwards. A
// replica may restore from that image only while the two agree, because a
// Firecracker memory image cannot be loaded into a differently-sized VM. When
// they disagree the replica boots from disk, which is slower and correct.
type ServiceSize struct {
	ServiceID   string
	VCPUs       int
	MemMiB      int
	ImageVCPUs  int
	ImageMemMiB int
	UpdatedAt   int64
}

// Size is the ServiceSize as a replica is created at, with the defaults
// already applied, so no caller has to remember what an absent row means.
func (s *ServiceSize) Size() (vcpus, memMiB int) {
	if s == nil {
		return DefaultServiceVCPUs, DefaultServiceMemMiB
	}
	vcpus, memMiB = s.VCPUs, s.MemMiB
	if vcpus <= 0 {
		vcpus = DefaultServiceVCPUs
	}
	if memMiB <= 0 {
		memMiB = DefaultServiceMemMiB
	}
	return vcpus, memMiB
}

// ImageMatchesSize reports whether the release's memory image was photographed
// at the size replicas are created at now. False means a replica must boot
// rather than restore.
func (s *ServiceSize) ImageMatchesSize() bool {
	if s == nil {
		// Nothing recorded: every replica is the default size and every image
		// was photographed at it, which is how it worked before the table.
		return true
	}
	vcpus, memMiB := s.Size()
	return s.ImageVCPUs == vcpus && s.ImageMemMiB == memMiB
}

// VolumePolicy is how often a volume is snapshotted and how much is kept.
//
// Two retention numbers rather than one, because the two questions are
// different: how far back at a day's resolution, and how far back at all.
// Keeping the newest KeepDaily snapshots plus the newest of each of the last
// KeepWeekly ISO weeks answers both in bounded space.
type VolumePolicy struct {
	VolumeID string
	// Cron is five fields in UTC, or @hourly/@daily/@weekly/@monthly. Empty
	// means no schedule, which is what every volume had before this existed.
	Cron       string
	KeepDaily  int
	KeepWeekly int
	UpdatedAt  int64
}

// Scheduled returns whether this policy actually schedules anything.
func (p *VolumePolicy) Scheduled() bool { return p != nil && p.Cron != "" }

// Lineage is where a forked machine came from.
//
// The build ids are the load-bearing part. A fork faults pages out of its
// parent's memory image until its own first suspend writes one, so the
// parent's next suspend or destroy must not discard an artifact a live fork is
// still reading. This row is what says so.
//
// Write-once: a fork's origin does not change.
type Lineage struct {
	ID             string
	ParentID       string
	CheckpointID   string
	MemBuildID     string
	RootfsBuildID  string
	VolumeSnapshot string
	CreatedAt      int64
}

// Handoff is one host offering a machine to another, on a planned drain.
//
// WRITE-ONCE. A CRDT merge has nothing to corrupt in a row nobody rewrites,
// which is most of why this exception is safe where an ordinary cross-host
// write is not. Repeated offers of one machine are new rows with a higher Seq,
// never an edit of the old one.
type Handoff struct {
	ID        string
	MachineID string
	FromHost  string
	ToHost    string
	Seq       int
	CreatedAt int64
}

type MachineCPU struct {
	ID          string
	Kind        string
	Vendor      string
	LastStart   string // machines only
	LastStartAt int64
	UpdatedAt   int64
}

// Volume is a persistent disk: a JuiceFS filesystem holding one raw ext4
// image, handed to a machine as a second virtio-blk drive.
//
// HostID is the writer and the mount lock at once. The metadata engine behind
// a volume is a single SQLite file, so two hosts mounting one corrupts it; the
// row naming an owner is what keeps that from happening, and it moves only
// when the machine does.
type Volume struct {
	ID        string
	Name      string
	MachineID string
	SizeMiB   int
	S3Prefix  string
	MountPath string
	HostID    string
	CreatedAt int64
}

// Service is a set of machines sharing a name, a release and an environment.
//
// Only Env and EnvSealed are consumed in this phase; the rest of the row is
// the rollout shape, which lands with the schema because adding a column later
// is a fleet-wide re-bootstrap rather than a migration.
// Release is one deployable version of a service.
//
// The build pair is the point. rootfs_build_id is what the FIRST replica of
// this release boots from; mem_build_id is stamped after that replica passes
// its health gate and is checkpointed, and every replica after it restores
// from the pair. A release with no mem build yet is not broken -- it is a
// release whose first replica has not finished proving itself, and callers
// fall back to booting.
type Release struct {
	ID            string
	ServiceID     string
	RootfsBuildID string
	// MemBuildID is empty until the first replica of this release has passed
	// its health gate and been checkpointed.
	MemBuildID string
	// Healthy records that a replica of this release reached its health gate
	// at least once. A release that never did is never a rollback target.
	Healthy   bool
	CreatedAt int64
}

// ReleaseSnapshot names the vmstate a release restores from.
//
// A release carries two build ids, its memory and its disk. Restoring needs a
// third artifact: the Firecracker vmstate, device state and vcpu registers,
// kilobytes beside gigabytes. It is stored under the machine and checkpoint it
// was captured from, and the release row names neither, so this row is the
// only way back to it.
//
// Write-once: a release is photographed once, and re-pointing one at a
// different vmstate would restore a guest whose registers describe a different
// machine than its memory does.
type ReleaseSnapshot struct {
	ID           string // the release id
	MachineID    string // the replica that was photographed
	CheckpointID string
	CreatedAt    int64
}

// Domain is a custom hostname pointed at a service.
type Domain struct {
	Hostname  string
	ServiceID string
	// VerifiedAt is when the CNAME was last observed pointing at this fleet.
	// Zero means unverified, and an unverified domain never gets a
	// certificate -- issuing for a name whose owner has not proved they want
	// it here burns the fleet's rate limit on someone else's typo.
	VerifiedAt int64
	CreatedAt  int64
}

// ServiceVolume is one volume a service mounts. Keyed by service and ordinal
// so a later volume-per-replica shape is more rows rather than a column add on
// a table that carries rows. Today the ordinal is always 1: a volume is
// mounted by one machine, so a service that mounts one runs one replica.
type ServiceVolume struct {
	ServiceID string
	Ordinal   int
	VolumeID  string
	CreatedAt int64
}

// ServiceVolumeID is the row key, <service_id>/<ordinal>.
func ServiceVolumeID(serviceID string, ordinal int) string {
	return serviceID + "/" + strconv.Itoa(ordinal)
}

type Service struct {
	ID        string
	Name      string
	App       string
	ReleaseID string
	Replicas  int
	Health    string // json, tagged union
	// Env is the non-secret half, plain json. EnvSealed is the secret half as
	// one sealed blob -- never plaintext, because every row here gossips to
	// every host and lands in every backup.
	Env          string
	EnvSealed    string
	Domain       string
	CustomDomain string
	Repo         string
	Branch       string
	Autodeploy   bool
	CreatedAt    int64
}

// APIKey is a hashed credential. Hashes replicate to every host so that each
// one authenticates locally -- auth survives the loss of any host, including
// the one running the dashboard.
type APIKey struct {
	Hash      string
	OrgID     string
	Scopes    string
	CreatedAt int64
}

// APIKeyLimits is what a RESTRICTED key may do, beyond its scopes.
//
// A key minted through the OAuth consent screen carries the choice the human
// made there: this agent, these machines, this long. A key with no row is
// unrestricted, which is every key an operator mints from the tokens page and
// every key that existed before the table did.
//
// Write-once, keyed by the key's own hash: a limit that could be widened
// later is not a limit. See schema.sql.
type APIKeyLimits struct {
	Hash string
	// NamePrefix is what every machine and service this key names must start
	// with. Empty means no naming restriction.
	NamePrefix string
	// MaxMachines caps how many machines carrying that prefix may exist at
	// once. 0 means no cap.
	MaxMachines int
	// ExpiresAt is when the key stops authenticating, in unix seconds. 0
	// means it lives until it is revoked.
	ExpiresAt int64
	CreatedAt int64
}

// Restricted reports whether these limits constrain anything at all.
func (l *APIKeyLimits) Restricted() bool {
	return l != nil && (l.NamePrefix != "" || l.MaxMachines > 0 || l.ExpiresAt > 0)
}

// Tenancy names the org that owns one machine, service or volume.
//
// A row of its own rather than a column on the object, because adding a
// column to a replicated table that carries rows backfills every one of them
// fleet-wide. Written before the object it names and never changed, which is
// why any host may write it: a value written once cannot be merged into
// something nobody wrote.
type Tenancy struct {
	ID        string
	OrgID     string
	Kind      string // machine|service|volume
	CreatedAt int64
}

// RepoLink is one org's standing permission to have this fleet fetch one
// repository through its GitHub App.
//
// A row of its own, write-once, for the reasons the schema comment gives at
// length: it is the only thing that ties a caller to a repository, and reading
// that permission out of services.repo would take the answer from the very
// caller it constrains.
type RepoLink struct {
	ID          string // RepoLinkID(OrgID, Repo)
	OrgID       string
	Repo        string // owner/name, lowercased
	ConnectedAt int64
}

// NormalizeRepo folds a repository slug to the form the rows are keyed by.
//
// GitHub owner and repository names are case-insensitive, so `Acme/Shop` and
// `acme/shop` are one repository and must not be two rows: the second spelling
// would otherwise read back as unconnected and refuse a caller that has every
// right to build. One function, called on both the write and the read, because
// two normalisations would be exactly that bug.
func NormalizeRepo(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

// RepoLinkID is the primary key: <org_id>/<owner>/<name>, lowercased.
//
// Keyed by the pair and not by the repository alone, so two orgs may each be
// connected to one public repository and neither can squat on the other's
// name. See the schema comment on repo_links.
func RepoLinkID(orgID, repo string) string {
	return strings.ToLower(strings.TrimSpace(orgID)) + "/" + NormalizeRepo(repo)
}

// Revocation is a killed API key. Adding a row rather than deleting the key's,
// because a delete loses to a replica still carrying the insert and the
// credential comes back alive.
type Revocation struct {
	Hash      string
	RevokedAt int64
}

// Quota is one org's limits. Zero means "no row was ever written", and the
// caller applies its defaults; a limit of zero is expressed by writing the row
// with an explicit 0, which is how an org is frozen.
type Quota struct {
	OrgID        string
	MaxMachines  int
	MaxVCPUs     int
	MaxMemMiB    int
	MaxVolumeGiB int
	MaxBuilds    int
	UpdatedAt    int64
	// MaxSnapshotGiB is how much object storage this org's checkpoints may
	// hold. It lives in its OWN table (org_snapshot_quotas), because
	// org_quotas carries rows on every running fleet and a column add there is
	// the cr-sqlite backfill rule 6 forbids. GetQuota reads both and PutQuota
	// writes both, so a caller sees one quota.
	MaxSnapshotGiB int
}

// Store is the swappable state backend.
type Store interface {
	GetMachine(ctx context.Context, id string) (*Machine, error)
	ListMachines(ctx context.Context) ([]Machine, error)
	// PutMachine writes a machine's row. A replicated store REJECTS a write
	// to a machine this host does not own unless an option authorises it --
	// see WriteOption for why that check cannot live in review comments.
	PutMachine(ctx context.Context, m *Machine, opts ...WriteOption) error
	// DeleteMachine makes a machine invisible to every read. A replicated
	// store tombstones rather than deleting, because a delete racing an
	// update loses through the merge and the row comes back.
	DeleteMachine(ctx context.Context, id string) error
	// ClaimMachine takes ownership of a machine from a host that is provably
	// gone, writing the owner and the state TOGETHER. They must move as one:
	// merges are per column, so split across two writes a row can end up
	// owned by the rescuer while still reporting what its dead owner last
	// said about it.
	ClaimMachine(ctx context.Context, id, newHostID, newState string, opts ...WriteOption) error
	// TouchMachine updates only the activity columns. Callers recording use of
	// a machine must not write the whole row, or they clobber a concurrent
	// lifecycle change.
	TouchMachine(ctx context.Context, id string, now int64) error

	PutHost(ctx context.Context, h *Host) error
	ListHosts(ctx context.Context) ([]Host, error)

	// PutHostCPU writes what a host says about its own CPU. A replicated store
	// refuses a write about any host but itself, exactly as PutHost does.
	PutHostCPU(ctx context.Context, h *HostCPU) error
	ListHostCPU(ctx context.Context) ([]HostCPU, error)
	// PutHostCapacity records what a host can still hold. Written only by the
	// host it names, on its heartbeat.
	PutHostCapacity(ctx context.Context, c *HostCapacity) error
	ListHostCapacity(ctx context.Context) ([]HostCapacity, error)
	// PutHostBuilds records which builds a host has cached. Written only by
	// the host it names, and only when the set actually changed: this row is
	// gossiped in full on every write.
	PutHostBuilds(ctx context.Context, b *HostBuilds) error
	ListHostBuilds(ctx context.Context) ([]HostBuilds, error)

	// PutHandoff offers a machine to another host. Written once, by the
	// machine's CURRENT owner, and never updated: a repeated offer is a new
	// row with a higher Seq.
	// PutVolumePolicy records a volume's snapshot schedule and retention.
	// Written by the volume's host, which is the only one that can act on it.
	PutVolumePolicy(ctx context.Context, p *VolumePolicy) error
	// GetVolumePolicy returns ErrNotFound for a volume with no schedule,
	// which is every volume until somebody sets one.
	GetVolumePolicy(ctx context.Context, volumeID string) (*VolumePolicy, error)
	// ListVolumePolicies is every schedule, for the loop that fires them.
	ListVolumePolicies(ctx context.Context) ([]VolumePolicy, error)
	DeleteVolumePolicy(ctx context.Context, volumeID string) error

	// PutLineage records where a forked machine came from. Written once, by
	// the fork's own host.
	PutLineage(ctx context.Context, l *Lineage) error
	// GetLineage returns ErrNotFound for a machine that was not forked, which
	// is most of them.
	GetLineage(ctx context.Context, machineID string) (*Lineage, error)
	// ListLineage is every fork, for the check that keeps a parent's builds
	// alive while a fork still reads them.
	ListLineage(ctx context.Context) ([]Lineage, error)
	DeleteLineage(ctx context.Context, machineID string) error

	// PutReleaseSnapshot records which checkpoint's vmstate a release
	// restores from. Written once, by the host that photographed it.
	PutReleaseSnapshot(ctx context.Context, r *ReleaseSnapshot) error
	// GetReleaseSnapshot returns ErrNotFound for a release photographed
	// before this row existed, whose replicas boot rather than restore.
	GetReleaseSnapshot(ctx context.Context, releaseID string) (*ReleaseSnapshot, error)
	DeleteReleaseSnapshot(ctx context.Context, releaseID string) error

	PutHandoff(ctx context.Context, h *Handoff) error
	// NewestHandoff is the most recent offer of a machine, or ErrNotFound
	// when it has never been offered.
	NewestHandoff(ctx context.Context, machineID string) (*Handoff, error)
	// ListHandoffs is every offer, for the router's pending map and for the
	// reaper that clears old ones.
	ListHandoffs(ctx context.Context) ([]Handoff, error)
	DeleteHandoff(ctx context.Context, id string) error
	// PutMachineCPU records the vendor that photographed a memory image. The
	// writer is the host that writes the object row it describes, so a
	// replicated store runs the same owner check that row's write runs.
	PutMachineCPU(ctx context.Context, c *MachineCPU, opts ...WriteOption) error
	// GetMachineCPU returns ErrNotFound when nothing is recorded, which reads
	// as "in no pool" and ranks over the whole fleet.
	GetMachineCPU(ctx context.Context, id string) (*MachineCPU, error)
	// DeleteMachineCPU drops the pool record for an object that is being
	// removed. Every site that removes a machine or a checkpoint calls it:
	// this table is gossiped to the whole fleet AND materialized into a map in
	// every host's cache, so a row that outlives its object leaks in as many
	// places as there are hosts. A row that was never recorded is not an
	// error -- the caller asked for it to be gone and it is.
	//
	// It is called BEFORE the row it describes is removed, the reverse of the
	// write order: a replicated store resolves the writer by reading that row,
	// so deleting that row first makes this unauthorizable.
	DeleteMachineCPU(ctx context.Context, id string) error
	// PutLabels records the labels a machine or service was created with.
	// Written once, by the host that writes the object row, under the same
	// owner check; a replicated store refuses any other writer.
	PutLabels(ctx context.Context, l *Labels, opts ...WriteOption) error
	// GetLabels returns ErrNotFound when none were recorded, which callers
	// read as "no labels" rather than as a failure.
	GetLabels(ctx context.Context, id string) (*Labels, error)
	// DeleteLabels drops the row for an object being removed, before the
	// object row itself for the same reason DeleteMachineCPU is.
	DeleteLabels(ctx context.Context, id string) error
	// PutURLAuth records who may reach an object's URL: "public" or "org".
	// Same writer and check as PutLabels.
	PutURLAuth(ctx context.Context, u *URLAuth, opts ...WriteOption) error
	// GetURLAuth returns ErrNotFound when nothing is recorded, which the
	// router reads as public -- what every URL was before the table existed.
	GetURLAuth(ctx context.Context, id string) (*URLAuth, error)
	DeleteURLAuth(ctx context.Context, id string) error
	// PutBrokerGrant records what a machine or service may ask the broker for.
	// Replace semantics: the grant handed in is the whole grant, because a
	// merge of two partial grants is a permission nobody wrote.
	PutBrokerGrant(ctx context.Context, g *BrokerGrant, opts ...WriteOption) error
	// GetBrokerGrant returns ErrNotFound when nothing is granted, which every
	// caller must read as DENY rather than as an error to report.
	GetBrokerGrant(ctx context.Context, id string) (*BrokerGrant, error)
	DeleteBrokerGrant(ctx context.Context, id string) error

	// PutServiceSize records how big a service's replicas are. Written by the
	// service's arbiter, the host that already writes the services row, so the
	// merge has one writer.
	PutServiceSize(ctx context.Context, s *ServiceSize, opts ...WriteOption) error
	// GetServiceSize returns ErrNotFound when nothing is recorded, which reads
	// as the defaults -- what every service was before the table existed.
	GetServiceSize(ctx context.Context, serviceID string) (*ServiceSize, error)
	// DeleteServiceSize drops the row for a service being removed, before the
	// service row itself, for the reason DeleteLabels is.
	DeleteServiceSize(ctx context.Context, serviceID string) error

	// PutHostEgress records the prefix this host hands egress addresses out
	// of. Written only by the host it names.
	PutHostEgress(ctx context.Context, e *HostEgress, opts ...WriteOption) error
	// ListHostEgress is every host that manages egress, for the route that
	// reports a tenant every address they might leave from.
	ListHostEgress(ctx context.Context) ([]HostEgress, error)
	// GetHostEgress returns ErrNotFound for a host that manages none, which
	// reads as "the shared host address" -- what every host did before.
	GetHostEgress(ctx context.Context, hostID string) (*HostEgress, error)
	DeleteHostEgress(ctx context.Context, hostID string) error

	PutCheckpoint(ctx context.Context, c *Checkpoint) error
	ListCheckpoints(ctx context.Context, machineID string) ([]Checkpoint, error)
	DeleteCheckpoint(ctx context.Context, id string) error

	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	PutAPIKey(ctx context.Context, k *APIKey) error
	// ListAPIKeys returns one org's keys. Hashes only -- the plaintext exists
	// for the length of the mint response and is never stored anywhere.
	ListAPIKeys(ctx context.Context, orgID string) ([]APIKey, error)

	// PutTenancy records which org owns an object. Write-once in both
	// drivers: a second call with a different org leaves the first in place,
	// so a replayed create cannot move an object between tenants.
	PutTenancy(ctx context.Context, t *Tenancy) error
	// GetTenancy returns ErrNotFound for an object created before tenancy
	// existed. Callers treat that as admin-only rather than as public.
	GetTenancy(ctx context.Context, id string) (*Tenancy, error)
	// ListTenancy returns every row. The quota counter reads it once per
	// create, joined against the object tables in memory.
	ListTenancy(ctx context.Context) ([]Tenancy, error)

	// PutRepoLink connects a repository to an org. Write-once in both
	// drivers, for the reason PutTenancy is: the row is what makes "any host
	// may write this" safe, and it is only safe while nothing can change a
	// value already written.
	PutRepoLink(ctx context.Context, l *RepoLink) error
	// GetRepoLink answers "may this org fetch this repository?" from LOCAL
	// state. ErrNotFound is the answer "no claim on record", which the API
	// turns into a 403 that says how to connect it. It is on the request path
	// of every {repo, ref} build, so it must never make a network call.
	GetRepoLink(ctx context.Context, orgID, repo string) (*RepoLink, error)
	// ListRepoLinks returns one org's connected repositories, or every row
	// when orgID is empty, which is what an admin key with no ?org= sees.
	ListRepoLinks(ctx context.Context, orgID string) ([]RepoLink, error)

	// PutRevocation tombstones a key. Write-once, and never paired with a
	// delete: see the Revocation type.
	PutRevocation(ctx context.Context, rv *Revocation) error
	// IsRevoked answers from the local replica, on every authenticated
	// request. It must never make a network call.
	IsRevoked(ctx context.Context, hash string) (bool, error)
	// GetRevocation returns WHEN a key was killed, for the key list. Separate
	// from IsRevoked because the request path asks only whether, and paying
	// for a row scan on every authenticated call to answer a question no
	// caller there asks would be a cost with no reader.
	GetRevocation(ctx context.Context, hash string) (*Revocation, error)

	// PutAPIKeyLimits records what a restricted key may do. Write-once in
	// both drivers, for the reason PutRevocation is: any host may write it
	// only while nothing can change a value already written, and a limit that
	// a later write could widen would not be one.
	PutAPIKeyLimits(ctx context.Context, l *APIKeyLimits) error
	// GetAPIKeyLimits answers from the local replica. ErrNotFound is the
	// answer "unrestricted", which is the common case, so this is on the
	// request path and must never make a network call.
	GetAPIKeyLimits(ctx context.Context, hash string) (*APIKeyLimits, error)

	// GetQuota returns ErrNotFound when the org has no row, which means the
	// defaults apply.
	GetQuota(ctx context.Context, orgID string) (*Quota, error)
	PutQuota(ctx context.Context, q *Quota) error

	GetVolume(ctx context.Context, id string) (*Volume, error)
	ListVolumes(ctx context.Context) ([]Volume, error)
	// PutVolume writes a volume's row. Like a machine, a volume belongs to one
	// host, and a replicated store REJECTS a write from anyone else -- the
	// exception being a rescuer taking it from an owner that has stopped
	// heartbeating, which needs WithDeadOwnerClaim. Two hosts believing they
	// own a volume is not a bookkeeping error: they both mount its metadata
	// database and destroy it.
	PutVolume(ctx context.Context, v *Volume, opts ...WriteOption) error

	// GetService reads a service row. ErrNotFound means the machine carries a
	// service_id whose row has not arrived yet, which on a create is normal
	// for a moment and never for long.
	GetService(ctx context.Context, id string) (*Service, error)
	// PutService writes one. Nothing in the schema can enforce single-writer
	// here -- a service row names no host -- so the rule is kept at the call
	// site: only the host that owns a service's machines writes it.
	PutService(ctx context.Context, svc *Service) error
	// DeleteService removes one. A service row carries the machine's sealed
	// environment, so a row left behind after its last machine is destroyed is
	// a secret replicated to every host in the fleet, forever, for a machine
	// that no longer exists.
	DeleteService(ctx context.Context, id string) error
	// CASServiceRelease flips a service to a new release only if it still
	// carries the one the caller last saw. The deploy path's one genuinely
	// corrupting race is two deploys interleaving, which leaves a service
	// naming one release while another's machines are the ones running.
	CASServiceRelease(ctx context.Context, id, from, to string) error
	// ListServices returns every service row. Reads are local and cheap; the
	// rollout and autoscale loops run on this.
	ListServices(ctx context.Context) ([]Service, error)
	// ListServiceNames returns only id, name and app -- what .internal needs
	// to turn a service name into its replicas. The full row carries the
	// sealed environment, and the resolver runs per DNS query; deserializing
	// every app's secrets on that path is a cost with no reader.
	ListServiceNames(ctx context.Context) ([]Service, error)

	// GetDomain reads one custom hostname. PutDomain writes one, DeleteDomain
	// removes it, and ListDomains is what the router and the TLS decision
	// function both read -- from the local replica, so a handshake costs a map
	// lookup rather than a query.
	GetDomain(ctx context.Context, hostname string) (*Domain, error)
	PutDomain(ctx context.Context, d *Domain) error
	DeleteDomain(ctx context.Context, hostname string) error
	ListDomains(ctx context.Context) ([]Domain, error)

	// ServiceVolume reads the volume a service mounts, ErrNotFound when it
	// mounts none.
	ServiceVolume(ctx context.Context, serviceID string) (*ServiceVolume, error)
	// PutServiceVolume writes the binding. Write-once, from the service's
	// arbiter: the row names a service, so there is no host column to enforce
	// single-writer on and nothing a last-write-wins merge could corrupt.
	PutServiceVolume(ctx context.Context, sv *ServiceVolume) error
	// DeleteServiceVolume drops ONE ordinal's binding, for a scale down. The
	// volume itself is destroyed by the caller: this row is the name, not the
	// thing, and deleting a row that still names a live volume would leak it.
	DeleteServiceVolume(ctx context.Context, serviceID string, ordinal int) error
	// DeleteServiceVolumes drops a service's bindings, called beside
	// DeleteService. The volume itself stays.
	DeleteServiceVolumes(ctx context.Context, serviceID string) error
	// ListServiceVolumes returns every binding -- what a create reads to
	// refuse a volume another service already mounts.
	ListServiceVolumes(ctx context.Context) ([]ServiceVolume, error)

	// GetRelease reads one release. PutRelease writes one.
	//
	// A release is written only by the host deploying it, which is the same
	// host the service arbiter chose -- releases inherit the service's writer
	// rather than needing an arbiter of their own.
	GetRelease(ctx context.Context, id string) (*Release, error)
	PutRelease(ctx context.Context, r *Release) error
	// ReleasesFor returns a service's releases, newest first. The rollback
	// target is the newest healthy release that is not the current one.
	ReleasesFor(ctx context.Context, serviceID string) ([]Release, error)
	// GetTemplate reads the fleet's golden template. ErrNotFound means no host
	// has built one yet.
	GetTemplate(ctx context.Context, id string) (*Template, error)
	PutTemplate(ctx context.Context, t *Template) error

	Close() error
}

type sqliteStore struct{ db *sql.DB }

// Open returns a SQLite-backed Store with the schema applied.
func Open(dsn string) (Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open %q: %w", dsn, err)
	}
	// One connection, deliberately. This driver sets no busy timeout, so
	// concurrent writers to a single SQLite file hit SQLITE_BUSY; serializing
	// avoids it. It also keeps ":memory:" coherent -- an in-memory database is
	// private to its connection, so a pooled second one would see empty tables.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(Schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("state: apply schema: %w", err)
	}
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

// addMissingColumns brings an existing database up to the declared schema.
//
// Schema is CREATE TABLE IF NOT EXISTS throughout, which is what Corrosion
// needs -- it loads the same file at agent start and cr-sqlite reconciles the
// declared shape against the database. SQLite does not: IF NOT EXISTS skips
// the whole statement when the table exists, so a column added to a table that
// a host already has is simply never created, and the first query naming it
// fails with "no such column" on a host that upgraded in place.
//
// So the column adds live here rather than in Schema. They must NOT go in the
// .sql file: Corrosion reads it too, and a bare ALTER there would either be
// rejected or replicate DDL, which is the fleet-wide backfill this project
// otherwise goes out of its way to avoid.
//
// Each entry is idempotent by inspection of the table, so this is safe to run
// on every Open, which is when it runs.
func addMissingColumns(db *sql.DB) error {
	wanted := []struct{ table, column, decl string }{
		// Added by 5c so a release can name the memory build its replicas
		// restore from.
		{"releases", "mem_build_id", "TEXT"},
	}
	for _, w := range wanted {
		has, err := hasColumn(db, w.table, w.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec("ALTER TABLE " + w.table + " ADD COLUMN " + w.column + " " + w.decl); err != nil {
			return fmt.Errorf("state: add %s.%s: %w", w.table, w.column, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("SELECT 1 FROM pragma_table_info(?) WHERE name = ?", table, column)
	if err != nil {
		return false, fmt.Errorf("state: inspect %s: %w", table, err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }

const machineCols = `id, name, host_id, state, kind_knobs, image_ref, vcpus, mem_mib,
	domain, custom_domain, app_port, agent_port, agent_token_hash,
	mem_build_id, rootfs_build_id, template_mem_build_id, template_rootfs_build_id,
	volume_id, service_id, release_id, app, slot,
	last_activity, updated_at`

func scanMachine(sc interface{ Scan(...any) error }) (*Machine, error) {
	var m Machine
	err := sc.Scan(&m.ID, &m.Name, &m.HostID, &m.State, &m.KindKnobs, &m.ImageRef,
		&m.VCPUs, &m.MemMiB, &m.Domain, &m.CustomDomain, &m.AppPort, &m.AgentPort,
		&m.AgentTokenHash, &m.MemBuildID, &m.RootfsBuildID,
		&m.TemplateMemBuildID, &m.TemplateRootfsBuildID, &m.VolumeID,
		&m.ServiceID, &m.ReleaseID, &m.App, &m.Slot, &m.LastActivity, &m.UpdatedAt)
	return &m, err
}

func (s *sqliteStore) GetMachine(ctx context.Context, id string) (*Machine, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+machineCols+` FROM machines WHERE id = ?`, id)
	m, err := scanMachine(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get machine %q: %w", id, err)
	}
	return m, nil
}

func (s *sqliteStore) ListMachines(ctx context.Context) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+machineCols+` FROM machines ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list machines: %w", err)
	}
	defer rows.Close()

	var out []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan machine: %w", err)
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// PutMachine ignores the write options: a single-box store has exactly one
// writer, so there is no invariant here for them to guard.
func (s *sqliteStore) PutMachine(ctx context.Context, m *Machine, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machines (`+machineCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, host_id=excluded.host_id, state=excluded.state,
			kind_knobs=excluded.kind_knobs, image_ref=excluded.image_ref,
			vcpus=excluded.vcpus, mem_mib=excluded.mem_mib, domain=excluded.domain,
			custom_domain=excluded.custom_domain, app_port=excluded.app_port,
			agent_port=excluded.agent_port, agent_token_hash=excluded.agent_token_hash,
			mem_build_id=excluded.mem_build_id, rootfs_build_id=excluded.rootfs_build_id,
			template_mem_build_id=excluded.template_mem_build_id,
			template_rootfs_build_id=excluded.template_rootfs_build_id,
			volume_id=excluded.volume_id, service_id=excluded.service_id,
			release_id=excluded.release_id, app=excluded.app, slot=excluded.slot,
			last_activity=excluded.last_activity,
			updated_at=excluded.updated_at`,
		m.ID, m.Name, m.HostID, m.State, m.KindKnobs, m.ImageRef, m.VCPUs, m.MemMiB,
		m.Domain, m.CustomDomain, m.AppPort, m.AgentPort, m.AgentTokenHash,
		m.MemBuildID, m.RootfsBuildID, m.TemplateMemBuildID, m.TemplateRootfsBuildID,
		m.VolumeID, m.ServiceID, m.ReleaseID, m.App, m.Slot,
		m.LastActivity, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put machine %q: %w", m.ID, err)
	}
	return nil
}

// templateDescriptor is the part of a template that must move as one value.
//
// The three fields are meaningless apart: a memory build, the disk build it
// was captured beside, and the vmstate that ties them together. Storing them
// in one column is what keeps a merge from inventing a combination no host
// ever published. See the comment on the templates table.
type templateDescriptor struct {
	MemBuildID    string `json:"mem_build_id"`
	RootfsBuildID string `json:"rootfs_build_id"`
	SnapKey       string `json:"snap_key"`
}

// MarshalDescriptor renders the inseparable part of a template for storage.
func MarshalDescriptor(t *Template) (string, error) {
	b, err := json.Marshal(templateDescriptor{
		MemBuildID: t.MemBuildID, RootfsBuildID: t.RootfsBuildID, SnapKey: t.SnapKey,
	})
	if err != nil {
		return "", fmt.Errorf("state: encode template %q: %w", t.ID, err)
	}
	return string(b), nil
}

// UnmarshalDescriptor fills a template from a stored descriptor.
func UnmarshalDescriptor(t *Template, descriptor string) error {
	var d templateDescriptor
	if err := json.Unmarshal([]byte(descriptor), &d); err != nil {
		return fmt.Errorf("state: template %q has an unreadable descriptor: %w", t.ID, err)
	}
	t.MemBuildID, t.RootfsBuildID, t.SnapKey = d.MemBuildID, d.RootfsBuildID, d.SnapKey
	return nil
}

func (s *sqliteStore) GetTemplate(ctx context.Context, id string) (*Template, error) {
	var t Template
	var descriptor string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, descriptor, created_at FROM templates WHERE id = ?`, id).
		Scan(&t.ID, &descriptor, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("state: template %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("state: get template %q: %w", id, err)
	}
	if err := UnmarshalDescriptor(&t, descriptor); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *sqliteStore) PutTemplate(ctx context.Context, t *Template) error {
	descriptor, err := MarshalDescriptor(t)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO templates (id, descriptor, created_at)
		VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			descriptor=excluded.descriptor, created_at=excluded.created_at`,
		t.ID, descriptor, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put template %q: %w", t.ID, err)
	}
	return nil
}

// ClaimMachine writes the owner and the state together. On a single box this
// is only ever the local host reclaiming its own row after a restart.
func (s *sqliteStore) ClaimMachine(ctx context.Context, id, newHostID, newState string, _ ...WriteOption) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE machines SET host_id = ?, state = ?, updated_at = ? WHERE id = ?`,
		newHostID, newState, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("state: claim machine %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: claim machine %q: %w", id, ErrNotFound)
	}
	return nil
}

func (s *sqliteStore) DeleteMachine(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete machine %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) TouchMachine(ctx context.Context, id string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE machines SET last_activity = ?, updated_at = ? WHERE id = ?`, now, now, id)
	if err != nil {
		return fmt.Errorf("state: touch machine %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) PutHost(ctx context.Context, h *Host) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO hosts (id, wg_addr, public_ip, cpu_free, mem_free_mib, last_seen)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			wg_addr=excluded.wg_addr, public_ip=excluded.public_ip,
			cpu_free=excluded.cpu_free, mem_free_mib=excluded.mem_free_mib,
			last_seen=excluded.last_seen`,
		h.ID, h.WGAddr, h.PublicIP, h.CPUFree, h.MemFreeMiB, h.LastSeen)
	if err != nil {
		return fmt.Errorf("state: put host %q: %w", h.ID, err)
	}
	return nil
}

func (s *sqliteStore) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.wg_addr, h.public_ip, h.cpu_free, h.mem_free_mib, h.last_seen,
			COALESCE(c.vendor, '')
		FROM hosts h LEFT JOIN host_cpu c ON c.host_id = h.id
		ORDER BY h.id`)
	if err != nil {
		return nil, fmt.Errorf("state: list hosts: %w", err)
	}
	defer rows.Close()

	var out []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.WGAddr, &h.PublicIP, &h.CPUFree, &h.MemFreeMiB, &h.LastSeen, &h.Vendor); err != nil {
			return nil, fmt.Errorf("state: scan host: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PutHostCPU(ctx context.Context, h *HostCPU) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_cpu (host_id, vendor, cpu_template, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			vendor=excluded.vendor, cpu_template=excluded.cpu_template,
			updated_at=excluded.updated_at`,
		h.HostID, h.Vendor, h.CPUTemplate, h.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put host cpu %q: %w", h.HostID, err)
	}
	return nil
}

func (s *sqliteStore) ListHostCPU(ctx context.Context) ([]HostCPU, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_id, vendor, cpu_template, updated_at FROM host_cpu ORDER BY host_id`)
	if err != nil {
		return nil, fmt.Errorf("state: list host cpu: %w", err)
	}
	defer rows.Close()

	var out []HostCPU
	for rows.Next() {
		var h HostCPU
		if err := rows.Scan(&h.HostID, &h.Vendor, &h.CPUTemplate, &h.UpdatedAt); err != nil {
			return nil, fmt.Errorf("state: scan host cpu: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PutHostCapacity(ctx context.Context, c *HostCapacity) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_capacity (host_id, mem_free_mib, mem_reclaimable_mib,
			cpu_count, vcpus_running, draining, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			mem_free_mib=excluded.mem_free_mib,
			mem_reclaimable_mib=excluded.mem_reclaimable_mib,
			cpu_count=excluded.cpu_count, vcpus_running=excluded.vcpus_running,
			draining=excluded.draining, updated_at=excluded.updated_at`,
		c.HostID, c.MemFreeMiB, c.MemReclaimableMiB, c.CPUCount, c.VCPUsRunning,
		boolToInt(c.Draining), c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put host capacity %q: %w", c.HostID, err)
	}
	return nil
}

func (s *sqliteStore) ListHostCapacity(ctx context.Context) ([]HostCapacity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT host_id, mem_free_mib, mem_reclaimable_mib, cpu_count,
		       vcpus_running, draining, updated_at
		FROM host_capacity ORDER BY host_id`)
	if err != nil {
		return nil, fmt.Errorf("state: list host capacity: %w", err)
	}
	defer rows.Close()

	var out []HostCapacity
	for rows.Next() {
		var c HostCapacity
		var draining int
		if err := rows.Scan(&c.HostID, &c.MemFreeMiB, &c.MemReclaimableMiB,
			&c.CPUCount, &c.VCPUsRunning, &draining, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("state: scan host capacity: %w", err)
		}
		c.Draining = draining != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PutVolumePolicy(ctx context.Context, p *VolumePolicy) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO volume_policies (volume_id, cron, keep_daily, keep_weekly, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(volume_id) DO UPDATE SET
			cron=excluded.cron, keep_daily=excluded.keep_daily,
			keep_weekly=excluded.keep_weekly, updated_at=excluded.updated_at`,
		p.VolumeID, p.Cron, p.KeepDaily, p.KeepWeekly, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put volume policy %q: %w", p.VolumeID, err)
	}
	return nil
}

func (s *sqliteStore) GetVolumePolicy(ctx context.Context, volumeID string) (*VolumePolicy, error) {
	var p VolumePolicy
	err := s.db.QueryRowContext(ctx, `
		SELECT volume_id, cron, keep_daily, keep_weekly, updated_at
		FROM volume_policies WHERE volume_id = ?`, volumeID).
		Scan(&p.VolumeID, &p.Cron, &p.KeepDaily, &p.KeepWeekly, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get volume policy %q: %w", volumeID, err)
	}
	return &p, nil
}

func (s *sqliteStore) ListVolumePolicies(ctx context.Context) ([]VolumePolicy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT volume_id, cron, keep_daily, keep_weekly, updated_at
		FROM volume_policies ORDER BY volume_id`)
	if err != nil {
		return nil, fmt.Errorf("state: list volume policies: %w", err)
	}
	defer rows.Close()
	var out []VolumePolicy
	for rows.Next() {
		var p VolumePolicy
		if err := rows.Scan(&p.VolumeID, &p.Cron, &p.KeepDaily, &p.KeepWeekly, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteVolumePolicy(ctx context.Context, volumeID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM volume_policies WHERE volume_id = ?`, volumeID); err != nil {
		return fmt.Errorf("state: delete volume policy %q: %w", volumeID, err)
	}
	return nil
}

func (s *sqliteStore) PutLineage(ctx context.Context, l *Lineage) error {
	// INSERT, never upsert: a fork's origin does not change, and an upsert
	// here would let a later write rewrite which builds are pinned.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machine_lineage (id, parent_id, checkpoint_id, mem_build_id,
			rootfs_build_id, volume_snapshot, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.ParentID, l.CheckpointID, l.MemBuildID, l.RootfsBuildID,
		l.VolumeSnapshot, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put lineage %q: %w", l.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetLineage(ctx context.Context, machineID string) (*Lineage, error) {
	var l Lineage
	err := s.db.QueryRowContext(ctx, `
		SELECT id, parent_id, checkpoint_id, mem_build_id, rootfs_build_id,
		       volume_snapshot, created_at
		FROM machine_lineage WHERE id = ?`, machineID).
		Scan(&l.ID, &l.ParentID, &l.CheckpointID, &l.MemBuildID, &l.RootfsBuildID,
			&l.VolumeSnapshot, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get lineage %q: %w", machineID, err)
	}
	return &l, nil
}

func (s *sqliteStore) ListLineage(ctx context.Context) ([]Lineage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, parent_id, checkpoint_id, mem_build_id, rootfs_build_id,
		       volume_snapshot, created_at
		FROM machine_lineage ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list lineage: %w", err)
	}
	defer rows.Close()
	var out []Lineage
	for rows.Next() {
		var l Lineage
		if err := rows.Scan(&l.ID, &l.ParentID, &l.CheckpointID, &l.MemBuildID,
			&l.RootfsBuildID, &l.VolumeSnapshot, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteLineage(ctx context.Context, machineID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machine_lineage WHERE id = ?`, machineID); err != nil {
		return fmt.Errorf("state: delete lineage %q: %w", machineID, err)
	}
	return nil
}

func (s *sqliteStore) PutReleaseSnapshot(ctx context.Context, r *ReleaseSnapshot) error {
	// INSERT, never upsert, for the same reason lineage is not upserted: a
	// release's vmstate does not move, and an upsert would let a later write
	// point a release at registers belonging to a different capture.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO release_snapshots (release_id, machine_id, checkpoint_id, created_at)
		VALUES (?,?,?,?)`,
		r.ID, r.MachineID, r.CheckpointID, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put release snapshot %q: %w", r.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetReleaseSnapshot(ctx context.Context, releaseID string) (*ReleaseSnapshot, error) {
	var r ReleaseSnapshot
	err := s.db.QueryRowContext(ctx, `
		SELECT release_id, machine_id, checkpoint_id, created_at
		FROM release_snapshots WHERE release_id = ?`, releaseID).
		Scan(&r.ID, &r.MachineID, &r.CheckpointID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get release snapshot %q: %w", releaseID, err)
	}
	return &r, nil
}

func (s *sqliteStore) DeleteReleaseSnapshot(ctx context.Context, releaseID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM release_snapshots WHERE release_id = ?`, releaseID)
	if err != nil {
		return fmt.Errorf("state: delete release snapshot %q: %w", releaseID, err)
	}
	return nil
}

func (s *sqliteStore) PutHandoff(ctx context.Context, h *Handoff) error {
	// INSERT, never upsert. A handoff row is write-once, and an ON CONFLICT
	// here would quietly turn the one property that makes this exception safe
	// into a row two hosts could rewrite.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machine_handoffs (id, machine_id, from_host, to_host, seq, created_at)
		VALUES (?,?,?,?,?,?)`,
		h.ID, h.MachineID, h.FromHost, h.ToHost, h.Seq, h.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put handoff %q: %w", h.ID, err)
	}
	return nil
}

func (s *sqliteStore) NewestHandoff(ctx context.Context, machineID string) (*Handoff, error) {
	var h Handoff
	err := s.db.QueryRowContext(ctx, `
		SELECT id, machine_id, from_host, to_host, seq, created_at
		FROM machine_handoffs WHERE machine_id = ? ORDER BY seq DESC LIMIT 1`, machineID).
		Scan(&h.ID, &h.MachineID, &h.FromHost, &h.ToHost, &h.Seq, &h.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: newest handoff of %q: %w", machineID, err)
	}
	return &h, nil
}

func (s *sqliteStore) ListHandoffs(ctx context.Context) ([]Handoff, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, machine_id, from_host, to_host, seq, created_at
		FROM machine_handoffs ORDER BY machine_id, seq`)
	if err != nil {
		return nil, fmt.Errorf("state: list handoffs: %w", err)
	}
	defer rows.Close()
	var out []Handoff
	for rows.Next() {
		var h Handoff
		if err := rows.Scan(&h.ID, &h.MachineID, &h.FromHost, &h.ToHost, &h.Seq, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteHandoff(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machine_handoffs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete handoff %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) PutHostBuilds(ctx context.Context, b *HostBuilds) error {
	blob, err := encodeBuilds(b.Builds)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO host_builds (host_id, builds, updated_at) VALUES (?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			builds=excluded.builds, updated_at=excluded.updated_at`,
		b.HostID, blob, b.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put host builds %q: %w", b.HostID, err)
	}
	return nil
}

func (s *sqliteStore) ListHostBuilds(ctx context.Context) ([]HostBuilds, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_id, builds, updated_at FROM host_builds ORDER BY host_id`)
	if err != nil {
		return nil, fmt.Errorf("state: list host builds: %w", err)
	}
	defer rows.Close()

	var out []HostBuilds
	for rows.Next() {
		var b HostBuilds
		var blob string
		if err := rows.Scan(&b.HostID, &blob, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("state: scan host builds: %w", err)
		}
		b.Builds = decodeBuilds(blob)
		out = append(out, b)
	}
	return out, rows.Err()
}

// encodeBuilds caps and serialises the advertised build set. Capping here
// rather than at every call site means no caller can accidentally publish an
// unbounded row.
func encodeBuilds(ids []string) (string, error) {
	if len(ids) > MaxCachedBuildsPublished {
		ids = ids[:MaxCachedBuildsPublished]
	}
	if len(ids) == 0 {
		return "[]", nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("state: encode host builds: %w", err)
	}
	return string(blob), nil
}

// decodeBuilds reads it back. A row that cannot be read is an EMPTY set, not
// an error: the only thing it feeds is a placement bonus, and losing the bonus
// costs a download while failing the read would cost the create.
func decodeBuilds(blob string) []string {
	if blob == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(blob), &out); err != nil {
		return nil
	}
	return out
}

func (s *sqliteStore) PutMachineCPU(ctx context.Context, c *MachineCPU, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machine_cpu (id, kind, vendor, last_start, last_start_at, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, vendor=excluded.vendor, last_start=excluded.last_start,
			last_start_at=excluded.last_start_at, updated_at=excluded.updated_at`,
		c.ID, c.Kind, c.Vendor, c.LastStart, c.LastStartAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put machine cpu %q: %w", c.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetMachineCPU(ctx context.Context, id string) (*MachineCPU, error) {
	var c MachineCPU
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, vendor, last_start, last_start_at, updated_at FROM machine_cpu WHERE id = ?`,
		id).Scan(&c.ID, &c.Kind, &c.Vendor, &c.LastStart, &c.LastStartAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get machine cpu %q: %w", id, err)
	}
	return &c, nil
}

// DeleteMachineCPU on a single-host store has no owner to check: this host is
// the only writer of every row in it.
func (s *sqliteStore) DeleteMachineCPU(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machine_cpu WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete machine cpu %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) PutLabels(ctx context.Context, l *Labels, _ ...WriteOption) error {
	raw, err := json.Marshal(l.Labels)
	if err != nil {
		return fmt.Errorf("state: labels for %q: %w", l.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO machine_labels (id, kind, labels, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, labels=excluded.labels, updated_at=excluded.updated_at`,
		l.ID, l.Kind, string(raw), l.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put labels %q: %w", l.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetLabels(ctx context.Context, id string) (*Labels, error) {
	var l Labels
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, labels, updated_at FROM machine_labels WHERE id = ?`, id).
		Scan(&l.ID, &l.Kind, &raw, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get labels %q: %w", id, err)
	}
	if err := json.Unmarshal([]byte(raw), &l.Labels); err != nil {
		return nil, fmt.Errorf("state: labels %q are not a JSON object: %w", id, err)
	}
	return &l, nil
}

func (s *sqliteStore) DeleteLabels(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machine_labels WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete labels %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) PutURLAuth(ctx context.Context, u *URLAuth, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO url_auth (id, kind, mode, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, mode=excluded.mode, updated_at=excluded.updated_at`,
		u.ID, u.Kind, u.Mode, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put url auth %q: %w", u.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetURLAuth(ctx context.Context, id string) (*URLAuth, error) {
	var u URLAuth
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, mode, updated_at FROM url_auth WHERE id = ?`, id).
		Scan(&u.ID, &u.Kind, &u.Mode, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get url auth %q: %w", id, err)
	}
	return &u, nil
}

func (s *sqliteStore) DeleteURLAuth(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM url_auth WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete url auth %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) PutBrokerGrant(ctx context.Context, g *BrokerGrant, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO broker_grants (id, kind, org_id, scopes, sealed, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, org_id=excluded.org_id,
			scopes=excluded.scopes, sealed=excluded.sealed, updated_at=excluded.updated_at`,
		g.ID, g.Kind, g.OrgID, strings.Join(g.Scopes, ","), g.Sealed, g.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put broker grant %q: %w", g.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetBrokerGrant(ctx context.Context, id string) (*BrokerGrant, error) {
	var g BrokerGrant
	var scopes string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, org_id, scopes, sealed, updated_at FROM broker_grants WHERE id = ?`, id).
		Scan(&g.ID, &g.Kind, &g.OrgID, &scopes, &g.Sealed, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get broker grant %q: %w", id, err)
	}
	g.Scopes = SplitScopes(scopes)
	return &g, nil
}

func (s *sqliteStore) DeleteBrokerGrant(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM broker_grants WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete broker grant %q: %w", id, err)
	}
	return nil
}

// SplitScopes reads the stored csv, dropping empties.
//
// An empty string must become an EMPTY slice rather than a slice holding one
// empty scope: the latter is a grant that looks non-empty to every caller that
// checks length, which is the difference between deny and allow.
func SplitScopes(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s *sqliteStore) PutServiceSize(ctx context.Context, sz *ServiceSize, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_sizes (service_id, vcpus, mem_mib, image_vcpus, image_mem_mib, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(service_id) DO UPDATE SET
			vcpus=excluded.vcpus, mem_mib=excluded.mem_mib,
			image_vcpus=excluded.image_vcpus, image_mem_mib=excluded.image_mem_mib,
			updated_at=excluded.updated_at`,
		sz.ServiceID, sz.VCPUs, sz.MemMiB, sz.ImageVCPUs, sz.ImageMemMiB, sz.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put service size %q: %w", sz.ServiceID, err)
	}
	return nil
}

func (s *sqliteStore) GetServiceSize(ctx context.Context, serviceID string) (*ServiceSize, error) {
	var sz ServiceSize
	err := s.db.QueryRowContext(ctx, `
		SELECT service_id, vcpus, mem_mib, image_vcpus, image_mem_mib, updated_at
		FROM service_sizes WHERE service_id = ?`, serviceID).
		Scan(&sz.ServiceID, &sz.VCPUs, &sz.MemMiB, &sz.ImageVCPUs, &sz.ImageMemMiB, &sz.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get service size %q: %w", serviceID, err)
	}
	return &sz, nil
}

func (s *sqliteStore) DeleteServiceSize(ctx context.Context, serviceID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM service_sizes WHERE service_id = ?`, serviceID); err != nil {
		return fmt.Errorf("state: delete service size %q: %w", serviceID, err)
	}
	return nil
}

func (s *sqliteStore) PutHostEgress(ctx context.Context, e *HostEgress, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_egress (host_id, prefix6, interface, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			prefix6=excluded.prefix6, interface=excluded.interface, updated_at=excluded.updated_at`,
		e.HostID, e.Prefix6, e.Interface, e.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put host egress %q: %w", e.HostID, err)
	}
	return nil
}

func (s *sqliteStore) ListHostEgress(ctx context.Context) ([]HostEgress, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT host_id, prefix6, interface, updated_at FROM host_egress ORDER BY host_id`)
	if err != nil {
		return nil, fmt.Errorf("state: list host egress: %w", err)
	}
	defer rows.Close()
	var out []HostEgress
	for rows.Next() {
		var e HostEgress
		if err := rows.Scan(&e.HostID, &e.Prefix6, &e.Interface, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetHostEgress(ctx context.Context, hostID string) (*HostEgress, error) {
	var e HostEgress
	err := s.db.QueryRowContext(ctx, `
		SELECT host_id, prefix6, interface, updated_at FROM host_egress WHERE host_id = ?`, hostID).
		Scan(&e.HostID, &e.Prefix6, &e.Interface, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get host egress %q: %w", hostID, err)
	}
	return &e, nil
}

func (s *sqliteStore) DeleteHostEgress(ctx context.Context, hostID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM host_egress WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("state: delete host egress %q: %w", hostID, err)
	}
	return nil
}

func (s *sqliteStore) PutCheckpoint(ctx context.Context, c *Checkpoint) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkpoints (id, machine_id, seq, comment, source_id,
			mem_build_id, rootfs_build_id, durable, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			seq=excluded.seq, comment=excluded.comment, source_id=excluded.source_id,
			mem_build_id=excluded.mem_build_id, rootfs_build_id=excluded.rootfs_build_id,
			durable=excluded.durable, created_at=excluded.created_at`,
		c.ID, c.MachineID, c.Seq, c.Comment, c.SourceID,
		c.MemBuildID, c.RootfsBuildID, c.Durable, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put checkpoint %q: %w", c.ID, err)
	}
	return nil
}

func (s *sqliteStore) ListCheckpoints(ctx context.Context, machineID string) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, machine_id, seq, comment, source_id, mem_build_id,
			rootfs_build_id, durable, created_at
		FROM checkpoints WHERE machine_id = ? ORDER BY seq`, machineID)
	if err != nil {
		return nil, fmt.Errorf("state: list checkpoints for %q: %w", machineID, err)
	}
	defer rows.Close()

	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		if err := rows.Scan(&c.ID, &c.MachineID, &c.Seq, &c.Comment, &c.SourceID,
			&c.MemBuildID, &c.RootfsBuildID, &c.Durable, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: scan checkpoint: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteCheckpoint(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete checkpoint %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT hash, org_id, scopes, created_at FROM api_keys WHERE hash = ?`, hash)
	var k APIKey
	err := row.Scan(&k.Hash, &k.OrgID, &k.Scopes, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get api key: %w", err)
	}
	return &k, nil
}

func (s *sqliteStore) PutAPIKey(ctx context.Context, k *APIKey) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (hash, org_id, scopes, created_at) VALUES (?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET
			org_id=excluded.org_id, scopes=excluded.scopes, created_at=excluded.created_at`,
		k.Hash, k.OrgID, k.Scopes, k.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put api key: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListAPIKeys(ctx context.Context, orgID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash, org_id, scopes, created_at FROM api_keys
		 WHERE org_id = ? ORDER BY created_at DESC, hash`, orgID)
	if err != nil {
		return nil, fmt.Errorf("state: list api keys: %w", err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.Hash, &k.OrgID, &k.Scopes, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: scan api key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PutTenancy is DO NOTHING rather than DO UPDATE, and that is the whole
// invariant: the row is what makes "any host may write this" safe, and it is
// only safe while nothing can change a value that was already written.
func (s *sqliteStore) PutTenancy(ctx context.Context, t *Tenancy) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenancy (id, org_id, kind, created_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		t.ID, t.OrgID, t.Kind, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put tenancy %q: %w", t.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetTenancy(ctx context.Context, id string) (*Tenancy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, org_id, kind, created_at FROM tenancy WHERE id = ?`, id)
	var t Tenancy
	err := row.Scan(&t.ID, &t.OrgID, &t.Kind, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get tenancy %q: %w", id, err)
	}
	return &t, nil
}

func (s *sqliteStore) ListTenancy(ctx context.Context) ([]Tenancy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, kind, created_at FROM tenancy ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list tenancy: %w", err)
	}
	defer rows.Close()

	var out []Tenancy
	for rows.Next() {
		var t Tenancy
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Kind, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: scan tenancy: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const repoLinkCols = `id, org_id, repo, connected_at`

// PutRepoLink is DO NOTHING for the reason PutTenancy is: the row is only safe
// for any host to write while nothing can change a value already written.
func (s *sqliteStore) PutRepoLink(ctx context.Context, l *RepoLink) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repo_links (`+repoLinkCols+`) VALUES (?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		RepoLinkID(l.OrgID, l.Repo), l.OrgID, NormalizeRepo(l.Repo), l.ConnectedAt)
	if err != nil {
		return fmt.Errorf("state: put repo link %q: %w", l.Repo, err)
	}
	return nil
}

func (s *sqliteStore) GetRepoLink(ctx context.Context, orgID, repo string) (*RepoLink, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+repoLinkCols+` FROM repo_links WHERE id = ?`, RepoLinkID(orgID, repo))
	var l RepoLink
	err := row.Scan(&l.ID, &l.OrgID, &l.Repo, &l.ConnectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get repo link %q: %w", repo, err)
	}
	return &l, nil
}

func (s *sqliteStore) ListRepoLinks(ctx context.Context, orgID string) ([]RepoLink, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if orgID == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+repoLinkCols+` FROM repo_links ORDER BY id`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+repoLinkCols+` FROM repo_links WHERE org_id = ? ORDER BY id`, orgID)
	}
	if err != nil {
		return nil, fmt.Errorf("state: list repo links: %w", err)
	}
	defer rows.Close()

	var out []RepoLink
	for rows.Next() {
		var l RepoLink
		if err := rows.Scan(&l.ID, &l.OrgID, &l.Repo, &l.ConnectedAt); err != nil {
			return nil, fmt.Errorf("state: scan repo link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PutRevocation is DO NOTHING for the same reason PutTenancy is, plus one of
// its own: the earliest revocation time is the true one, and a re-revocation
// must not move it forward.
func (s *sqliteStore) PutRevocation(ctx context.Context, rv *Revocation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_key_revocations (hash, revoked_at) VALUES (?,?)
		ON CONFLICT(hash) DO NOTHING`, rv.Hash, rv.RevokedAt)
	if err != nil {
		return fmt.Errorf("state: put revocation: %w", err)
	}
	return nil
}

func (s *sqliteStore) GetRevocation(ctx context.Context, hash string) (*Revocation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT hash, revoked_at FROM api_key_revocations WHERE hash = ?`, hash)
	var rv Revocation
	err := row.Scan(&rv.Hash, &rv.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get revocation: %w", err)
	}
	return &rv, nil
}

const apiKeyLimitCols = `hash, name_prefix, max_machines, expires_at, created_at`

// PutAPIKeyLimits is DO NOTHING for the reason PutRevocation is, and for one
// of its own: the limits chosen when a key was minted are the only limits it
// ever has, so a second write must not be able to widen them.
func (s *sqliteStore) PutAPIKeyLimits(ctx context.Context, l *APIKeyLimits) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_key_limits (`+apiKeyLimitCols+`) VALUES (?,?,?,?,?)
		ON CONFLICT(hash) DO NOTHING`,
		l.Hash, l.NamePrefix, l.MaxMachines, l.ExpiresAt, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put api key limits: %w", err)
	}
	return nil
}

func (s *sqliteStore) GetAPIKeyLimits(ctx context.Context, hash string) (*APIKeyLimits, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+apiKeyLimitCols+` FROM api_key_limits WHERE hash = ?`, hash)
	var l APIKeyLimits
	err := row.Scan(&l.Hash, &l.NamePrefix, &l.MaxMachines, &l.ExpiresAt, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get api key limits: %w", err)
	}
	return &l, nil
}

func (s *sqliteStore) IsRevoked(ctx context.Context, hash string) (bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM api_key_revocations WHERE hash = ?`, hash)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state: check revocation: %w", err)
	}
	return true, nil
}

const quotaCols = `org_id, max_machines, max_vcpus, max_mem_mib, max_volume_gib, max_builds, updated_at`

func (s *sqliteStore) GetQuota(ctx context.Context, orgID string) (*Quota, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+quotaCols+` FROM org_quotas WHERE org_id = ?`, orgID)
	var q Quota
	err := row.Scan(&q.OrgID, &q.MaxMachines, &q.MaxVCPUs, &q.MaxMemMiB,
		&q.MaxVolumeGiB, &q.MaxBuilds, &q.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get quota %q: %w", orgID, err)
	}
	// The dual read. An org with a quota but no snapshot row is every org that
	// predates this table, and it gets zero here, which quota.Check reads as
	// "use the default" rather than as "refuse everything".
	var snap int
	if err := s.db.QueryRowContext(ctx,
		`SELECT max_snapshot_gib FROM org_snapshot_quotas WHERE org_id = ?`,
		orgID).Scan(&snap); err == nil {
		q.MaxSnapshotGiB = snap
	}
	return &q, nil
}

func (s *sqliteStore) PutQuota(ctx context.Context, q *Quota) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO org_quotas (`+quotaCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(org_id) DO UPDATE SET
			max_machines=excluded.max_machines, max_vcpus=excluded.max_vcpus,
			max_mem_mib=excluded.max_mem_mib, max_volume_gib=excluded.max_volume_gib,
			max_builds=excluded.max_builds, updated_at=excluded.updated_at`,
		q.OrgID, q.MaxMachines, q.MaxVCPUs, q.MaxMemMiB, q.MaxVolumeGiB,
		q.MaxBuilds, q.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put quota %q: %w", q.OrgID, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO org_snapshot_quotas (org_id, max_snapshot_gib, updated_at)
		VALUES (?,?,?)
		ON CONFLICT(org_id) DO UPDATE SET
			max_snapshot_gib=excluded.max_snapshot_gib,
			updated_at=excluded.updated_at`,
		q.OrgID, q.MaxSnapshotGiB, q.UpdatedAt); err != nil {
		return fmt.Errorf("state: put snapshot quota %q: %w", q.OrgID, err)
	}
	return nil
}

const volumeCols = `id, name, machine_id, size_mib, s3_prefix, mount_path, host_id, created_at`

func scanVolume(sc interface{ Scan(...any) error }) (*Volume, error) {
	var v Volume
	err := sc.Scan(&v.ID, &v.Name, &v.MachineID, &v.SizeMiB, &v.S3Prefix,
		&v.MountPath, &v.HostID, &v.CreatedAt)
	return &v, err
}

func (s *sqliteStore) GetVolume(ctx context.Context, id string) (*Volume, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+volumeCols+` FROM volumes WHERE id = ?`, id)
	v, err := scanVolume(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get volume %q: %w", id, err)
	}
	return v, nil
}

func (s *sqliteStore) ListVolumes(ctx context.Context) ([]Volume, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+volumeCols+` FROM volumes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list volumes: %w", err)
	}
	defer rows.Close()

	var out []Volume
	for rows.Next() {
		v, err := scanVolume(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan volume: %w", err)
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// PutVolume ignores the write options for the same reason PutMachine does: a
// single box has one writer, so there is no invariant here to guard.
func (s *sqliteStore) PutVolume(ctx context.Context, v *Volume, _ ...WriteOption) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO volumes (`+volumeCols+`)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, machine_id=excluded.machine_id,
			size_mib=excluded.size_mib, s3_prefix=excluded.s3_prefix,
			mount_path=excluded.mount_path, host_id=excluded.host_id,
			created_at=excluded.created_at`,
		v.ID, v.Name, v.MachineID, v.SizeMiB, v.S3Prefix, v.MountPath, v.HostID, v.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put volume %q: %w", v.ID, err)
	}
	return nil
}

const serviceCols = `id, name, app, release_id, replicas, health, env, env_sealed,
	domain, custom_domain, repo, branch, autodeploy, created_at`

func scanService(sc interface{ Scan(...any) error }) (*Service, error) {
	var svc Service
	err := sc.Scan(&svc.ID, &svc.Name, &svc.App, &svc.ReleaseID, &svc.Replicas,
		&svc.Health, &svc.Env, &svc.EnvSealed, &svc.Domain, &svc.CustomDomain,
		&svc.Repo, &svc.Branch, &svc.Autodeploy, &svc.CreatedAt)
	return &svc, err
}

func (s *sqliteStore) DeleteService(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete service %q: %w", id, err)
	}
	return nil
}

func (s *sqliteStore) GetService(ctx context.Context, id string) (*Service, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+serviceCols+` FROM services WHERE id = ?`, id)
	svc, err := scanService(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("state: service %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("state: get service %q: %w", id, err)
	}
	return svc, nil
}

func (s *sqliteStore) PutService(ctx context.Context, svc *Service) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO services (`+serviceCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, app=excluded.app, release_id=excluded.release_id,
			replicas=excluded.replicas, health=excluded.health, env=excluded.env,
			env_sealed=excluded.env_sealed, domain=excluded.domain,
			custom_domain=excluded.custom_domain, repo=excluded.repo,
			branch=excluded.branch, autodeploy=excluded.autodeploy,
			created_at=excluded.created_at`,
		svc.ID, svc.Name, svc.App, svc.ReleaseID, svc.Replicas, svc.Health,
		svc.Env, svc.EnvSealed, svc.Domain, svc.CustomDomain, svc.Repo,
		svc.Branch, svc.Autodeploy, svc.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put service %q: %w", svc.ID, err)
	}
	return nil
}

// releaseCols is the column list, in the order rows are scanned.
const releaseCols = `id, service_id, rootfs_build_id, mem_build_id, healthy, created_at`

func scanRelease(row interface{ Scan(...any) error }) (*Release, error) {
	var r Release
	var healthy int
	if err := row.Scan(&r.ID, &r.ServiceID, &r.RootfsBuildID, &r.MemBuildID,
		&healthy, &r.CreatedAt); err != nil {
		return nil, err
	}
	// SQLite has no bool, and corrosion hands back a JSON number for this
	// column -- the same shape the services.autodeploy scan already handles.
	r.Healthy = healthy != 0
	return &r, nil
}

func (s *sqliteStore) GetRelease(ctx context.Context, id string) (*Release, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE id = ?`, id)
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("state: release %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("state: get release %q: %w", id, err)
	}
	return r, nil
}

func (s *sqliteStore) PutRelease(ctx context.Context, r *Release) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO releases (`+releaseCols+`)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			service_id=excluded.service_id,
			rootfs_build_id=excluded.rootfs_build_id,
			mem_build_id=excluded.mem_build_id,
			healthy=excluded.healthy,
			created_at=excluded.created_at`,
		r.ID, r.ServiceID, r.RootfsBuildID, r.MemBuildID, boolToInt(r.Healthy), r.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put release %q: %w", r.ID, err)
	}
	return nil
}

func (s *sqliteStore) ReleasesFor(ctx context.Context, serviceID string) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+releaseCols+
		` FROM releases WHERE service_id = ? ORDER BY created_at DESC`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("state: releases for %q: %w", serviceID, err)
	}
	defer rows.Close()

	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan release: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+serviceCols+` FROM services`)
	if err != nil {
		return nil, fmt.Errorf("state: list services: %w", err)
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan service: %w", err)
		}
		out = append(out, *svc)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ListServiceNames(ctx context.Context) ([]Service, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, app FROM services`)
	if err != nil {
		return nil, fmt.Errorf("state: list service names: %w", err)
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		var svc Service
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.App); err != nil {
			return nil, fmt.Errorf("state: scan service name: %w", err)
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// boolToInt is the SQLite bool convention used throughout this file.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// OwnerFor names the ONE host responsible for a key, from the live set.
//
// The fleet has no coordinator, so every host has to reach the same answer
// from the same inputs or two of them act on one object. That is why the hash
// is FNV-1a over the key rather than Go's built-in map hash: the built-in is
// seeded per process, so two hosts running identical code would compute
// different buckets for the same key.
//
// It sorts its own input because rank is a position in a list, and two hosts
// that order the live set differently compute different owners from the same
// membership.
//
// Used for two things that must not diverge: which host rescues a machine
// whose owner died (selfheal.RescuerFor delegates here), and which host is
// allowed to write a service row -- services name machines rather than a host,
// so there is no column to guard single-writer on and the arbiter IS the
// guard.
func OwnerFor(key string, live []Host) (string, bool) {
	if len(live) == 0 {
		return "", false
	}
	sorted := append([]Host(nil), live...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return sorted[int(h.Sum32()%uint32(len(sorted)))].ID, true
}

// MachineOwnerFor names the ONE host that brings a machine back when its
// owner is gone. Same hash, same sort, same function as OwnerFor: only the
// candidate set differs. A memory image restores only on its own vendor, so
// the hosts of that vendor are ranked first; when none is live the whole
// live set is ranked and the winner boots the machine from its disk.
//
// A live host with no host_cpu row yet is in the full set and in no pool;
// every host writes host_cpu before its first heartbeat, so that window is
// liveness's own gossip-lag window and closes the same way.
//
// This is the MACHINE ranking only. The row writers (a service's arbiter, a
// domain's, a repo delivery's) keep calling OwnerFor over the unfiltered live
// set: partitioning an arbiter by vendor would give one row two writers and
// break single-writer through the merge, silently.
func MachineOwnerFor(machineID, vendor string, live []Host) (string, bool) {
	if vendor != "" {
		pool := live[:0:0]
		for _, h := range live {
			if h.Vendor == vendor {
				pool = append(pool, h)
			}
		}
		if owner, ok := OwnerFor(machineID, pool); ok {
			return owner, true
		}
	}
	return OwnerFor(machineID, live)
}

func (s *sqliteStore) CASServiceRelease(ctx context.Context, id, from, to string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE services SET release_id = ? WHERE id = ? AND release_id = ?`, to, id, from)
	if err != nil {
		return fmt.Errorf("state: flip service %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("state: service %q no longer carries release %q: %w",
			id, from, ErrNotOwner)
	}
	return nil
}

const domainCols = `hostname, service_id, verified_at, created_at`

func (s *sqliteStore) GetDomain(ctx context.Context, hostname string) (*Domain, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+domainCols+` FROM domains WHERE hostname = ?`, hostname)
	var d Domain
	err := row.Scan(&d.Hostname, &d.ServiceID, &d.VerifiedAt, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("state: domain %q: %w", hostname, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("state: get domain %q: %w", hostname, err)
	}
	return &d, nil
}

func (s *sqliteStore) PutDomain(ctx context.Context, d *Domain) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO domains (`+domainCols+`) VALUES (?,?,?,?)
		ON CONFLICT(hostname) DO UPDATE SET
			service_id=excluded.service_id, verified_at=excluded.verified_at,
			created_at=excluded.created_at`,
		d.Hostname, d.ServiceID, d.VerifiedAt, d.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put domain %q: %w", d.Hostname, err)
	}
	return nil
}

func (s *sqliteStore) DeleteDomain(ctx context.Context, hostname string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM domains WHERE hostname = ?`, hostname); err != nil {
		return fmt.Errorf("state: delete domain %q: %w", hostname, err)
	}
	return nil
}

func (s *sqliteStore) ListDomains(ctx context.Context) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+domainCols+` FROM domains`)
	if err != nil {
		return nil, fmt.Errorf("state: list domains: %w", err)
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Hostname, &d.ServiceID, &d.VerifiedAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const serviceVolumeCols = `id, service_id, ordinal, volume_id, created_at`

func (s *sqliteStore) DeleteServiceVolume(ctx context.Context, serviceID string, ordinal int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM service_volumes WHERE service_id = ? AND ordinal = ?`, serviceID, ordinal)
	if err != nil {
		return fmt.Errorf("state: unbind %s ordinal %d: %w", serviceID, ordinal, err)
	}
	return nil
}

func (s *sqliteStore) ServiceVolume(ctx context.Context, serviceID string) (*ServiceVolume, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+serviceVolumeCols+` FROM service_volumes WHERE service_id = ? ORDER BY ordinal`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("state: service volume %q: %w", serviceID, err)
	}
	defer rows.Close()
	var out []ServiceVolume
	for rows.Next() {
		var id string
		var sv ServiceVolume
		if err := rows.Scan(&id, &sv.ServiceID, &sv.Ordinal, &sv.VolumeID, &sv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(out) {
	case 0:
		return nil, fmt.Errorf("state: service volume %q: %w", serviceID, ErrNotFound)
	case 1:
		return &out[0], nil
	default:
		return nil, fmt.Errorf("state: service %q mounts %d volumes; this build supports one", serviceID, len(out))
	}
}

func (s *sqliteStore) PutServiceVolume(ctx context.Context, sv *ServiceVolume) error {
	// Write-once: a second writer cannot change a value even by accident,
	// which is what makes the arbiter's write safe under last-write-wins.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_volumes (`+serviceVolumeCols+`) VALUES (?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		ServiceVolumeID(sv.ServiceID, sv.Ordinal), sv.ServiceID, sv.Ordinal, sv.VolumeID, sv.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put service volume %q: %w", sv.ServiceID, err)
	}
	return nil
}

func (s *sqliteStore) DeleteServiceVolumes(ctx context.Context, serviceID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM service_volumes WHERE service_id = ?`, serviceID); err != nil {
		return fmt.Errorf("state: delete service volumes %q: %w", serviceID, err)
	}
	return nil
}

func (s *sqliteStore) ListServiceVolumes(ctx context.Context) ([]ServiceVolume, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+serviceVolumeCols+` FROM service_volumes`)
	if err != nil {
		return nil, fmt.Errorf("state: list service volumes: %w", err)
	}
	defer rows.Close()
	var out []ServiceVolume
	for rows.Next() {
		var id string
		var sv ServiceVolume
		if err := rows.Scan(&id, &sv.ServiceID, &sv.Ordinal, &sv.VolumeID, &sv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// NewOwnedID mints an id with the given prefix whose owner is this host.
//
// Ownership is hash(id) mod live_hosts, and some ids are minted by the host
// that will immediately write the row -- a service create being the case that
// forced this. On an N-host fleet a naive uuid gives the creating host a
// 1-in-N chance of being allowed to write what it just made, and forwarding
// cannot fix it the way it fixes a later write: the id does not exist yet, so
// a forwarded create would mint a DIFFERENT id on the far host and move the
// problem rather than solve it.
//
// So the id is chosen rather than accepted. This is create-time placement, the
// same shape as machine name allocation, and it states something true: the
// host that created a thing is the one that owns writing it. Ownership still
// moves with fleet membership afterwards, exactly as a machine's rescuer does
// -- the rule has to be agreed at any instant, not fixed forever.
//
// Expected attempts is the fleet size. The bound stops a host whose live set
// is momentarily empty or inconsistent from spinning; falling back to an
// unowned id is safe because the store's guard still decides, and will refuse.
func NewOwnedID(prefix, hostID string, live []Host) string {
	if len(live) <= 1 {
		return prefix + uuid.NewString()
	}
	for attempt := 0; attempt < 10*len(live); attempt++ {
		id := prefix + uuid.NewString()
		if owner, ok := OwnerFor(id, live); ok && owner == hostID {
			return id
		}
	}
	return prefix + uuid.NewString()
}

// LiveHosts filters a host list to those still heartbeating.
func LiveHosts(hosts []Host) []Host {
	out := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		if time.Since(time.Unix(h.LastSeen, 0)) < 90*time.Second {
			out = append(out, h)
		}
	}
	return out
}

// CurrentReplicas is the machines a request to a service's address may reach:
// this service's, on its current release, not a tombstone.
//
// A deploy is blue/green, so the previous release's machines are stopped but
// kept for a rollback. They must never be routed, or a service's address would
// answer from the release it was just moved off. Filtering on release_id is
// also what makes the cutover atomic for routing: the flip is one CAS on the
// service row, and every host's next resolve follows it.
func CurrentReplicas(svc Service, rows []Machine) []Machine {
	if svc.ReleaseID == "" {
		return nil
	}
	out := make([]Machine, 0, len(rows))
	for _, m := range rows {
		if IsCurrentReplica(svc, m) {
			out = append(out, m)
		}
	}
	return out
}

// IsCurrentReplica is the rule CurrentReplicas applies, for a caller holding a
// map rather than a slice.
//
// Exported so the subscription cache can filter its own machine map in place:
// the router asks it on every request to a service address, and materialising
// a slice of every machine in the fleet to hand to CurrentReplicas would put
// one fleet-sized allocation on the routing hot path.
func IsCurrentReplica(svc Service, m Machine) bool {
	return svc.ReleaseID != "" && m.ServiceID == svc.ID &&
		m.ReleaseID == svc.ReleaseID && m.State != StateDestroyed
}
