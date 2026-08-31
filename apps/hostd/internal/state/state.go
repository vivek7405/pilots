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
	"errors"
	"fmt"

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
	VolumeID       string
	ServiceID      string
	ReleaseID      string
	LastActivity   int64
	UpdatedAt      int64
}

// Host is one member of the fleet. Every host heartbeats its own row; a row
// whose last_seen is stale is what triggers self-heal on the survivors.
type Host struct {
	ID         string
	WGAddr     string
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
	PutMachine(ctx context.Context, m *Machine) error
	DeleteMachine(ctx context.Context, id string) error

	PutHost(ctx context.Context, h *Host) error
	ListHosts(ctx context.Context) ([]Host, error)

	PutCheckpoint(ctx context.Context, c *Checkpoint) error
	ListCheckpoints(ctx context.Context, machineID string) ([]Checkpoint, error)

	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	PutAPIKey(ctx context.Context, k *APIKey) error

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
	mem_build_id, rootfs_build_id, volume_id, service_id, release_id,
	last_activity, updated_at`

func scanMachine(sc interface{ Scan(...any) error }) (*Machine, error) {
	var m Machine
	err := sc.Scan(&m.ID, &m.Name, &m.HostID, &m.State, &m.KindKnobs, &m.ImageRef,
		&m.VCPUs, &m.MemMiB, &m.Domain, &m.CustomDomain, &m.AppPort, &m.AgentPort,
		&m.AgentTokenHash, &m.MemBuildID, &m.RootfsBuildID, &m.VolumeID,
		&m.ServiceID, &m.ReleaseID, &m.LastActivity, &m.UpdatedAt)
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

func (s *sqliteStore) PutMachine(ctx context.Context, m *Machine) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machines (`+machineCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, host_id=excluded.host_id, state=excluded.state,
			kind_knobs=excluded.kind_knobs, image_ref=excluded.image_ref,
			vcpus=excluded.vcpus, mem_mib=excluded.mem_mib, domain=excluded.domain,
			custom_domain=excluded.custom_domain, app_port=excluded.app_port,
			agent_port=excluded.agent_port, agent_token_hash=excluded.agent_token_hash,
			mem_build_id=excluded.mem_build_id, rootfs_build_id=excluded.rootfs_build_id,
			volume_id=excluded.volume_id, service_id=excluded.service_id,
			release_id=excluded.release_id, last_activity=excluded.last_activity,
			updated_at=excluded.updated_at`,
		m.ID, m.Name, m.HostID, m.State, m.KindKnobs, m.ImageRef, m.VCPUs, m.MemMiB,
		m.Domain, m.CustomDomain, m.AppPort, m.AgentPort, m.AgentTokenHash,
		m.MemBuildID, m.RootfsBuildID, m.VolumeID, m.ServiceID, m.ReleaseID,
		m.LastActivity, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put machine %q: %w", m.ID, err)
	}
	return nil
}

func (s *sqliteStore) DeleteMachine(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete machine %q: %w", id, err)
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
