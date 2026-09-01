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
//     allocation, self-heal claims of a provably dead host's machines, and the
//     dashboard host's api_keys writes.
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
	// APIKeyWrite: the dashboard's host writing api_keys, the one table whose
	// rows describe no machine.
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

// WithAPIKeyWrite authorises the dashboard host's api_keys writes.
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

// Template is the golden image machines are created from, shared by the whole
// fleet. See the schema for why it cannot be per host.
type Template struct {
	ID            string
	MemBuildID    string
	RootfsBuildID string
	SnapKey       string
	CreatedAt     int64
}

// GoldenTemplate is the id of the default template's row.
const GoldenTemplate = "golden"

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

	PutCheckpoint(ctx context.Context, c *Checkpoint) error
	ListCheckpoints(ctx context.Context, machineID string) ([]Checkpoint, error)
	DeleteCheckpoint(ctx context.Context, id string) error

	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	PutAPIKey(ctx context.Context, k *APIKey) error

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
	return &sqliteStore{db: db}, nil
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, wg_addr, public_ip, cpu_free, mem_free_mib, last_seen FROM hosts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list hosts: %w", err)
	}
	defer rows.Close()

	var out []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.WGAddr, &h.PublicIP, &h.CPUFree, &h.MemFreeMiB, &h.LastSeen); err != nil {
			return nil, fmt.Errorf("state: scan host: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
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
