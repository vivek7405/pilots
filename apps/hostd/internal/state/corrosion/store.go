package corrosion

import (
	"context"
	"fmt"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// deadAfter is how long a host may go without heartbeating before the fleet
// treats it as gone. It must match the self-heal loop's threshold: the driver
// re-checks it on a claim, and a disagreement would let a claim through that
// the rescue loop would not have made.
const deadAfter = 30 * time.Second

// Store is the cluster's machine state, replicated by Corrosion.
//
// Every host has a full local replica, so reads never leave the box. Writes go
// to the local agent and gossip outward, which is what removes the control
// plane -- and also what makes the single-writer rule load-bearing, since
// nothing in the merge can detect two hosts writing the same row.
type Store struct {
	client *Client
	hostID string
}

// NewStore wraps a client as the state store for hostID.
func NewStore(client *Client, hostID string) *Store {
	return &Store{client: client, hostID: hostID}
}

// machineCols is the column list, in the order rows are scanned.
const machineCols = `id, name, host_id, state, kind_knobs, image_ref, vcpus, mem_mib,
	domain, custom_domain, app_port, agent_port, agent_token_hash,
	mem_build_id, rootfs_build_id, template_mem_build_id, template_rootfs_build_id,
	volume_id, service_id, release_id, app, slot,
	last_activity, updated_at`

func scanMachine(rows *Rows, m *state.Machine) error {
	return rows.Scan(&m.ID, &m.Name, &m.HostID, &m.State, &m.KindKnobs, &m.ImageRef,
		&m.VCPUs, &m.MemMiB, &m.Domain, &m.CustomDomain, &m.AppPort, &m.AgentPort,
		&m.AgentTokenHash, &m.MemBuildID, &m.RootfsBuildID,
		&m.TemplateMemBuildID, &m.TemplateRootfsBuildID, &m.VolumeID,
		&m.ServiceID, &m.ReleaseID, &m.App, &m.Slot, &m.LastActivity, &m.UpdatedAt)
}

func (s *Store) GetMachine(ctx context.Context, id string) (*state.Machine, error) {
	// Tombstones are invisible: a destroyed machine is gone as far as every
	// caller is concerned, even though its row lingers until the reaper.
	rows, err := s.client.Query(ctx,
		`SELECT `+machineCols+` FROM machines WHERE id = ? AND state != ?`,
		id, state.StateDestroyed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: machine %q: %w", id, state.ErrNotFound)
	}
	var m state.Machine
	if err := scanMachine(rows, &m); err != nil {
		return nil, err
	}
	return &m, rows.Err()
}

func (s *Store) ListMachines(ctx context.Context) ([]state.Machine, error) {
	rows, err := s.client.Query(ctx,
		`SELECT `+machineCols+` FROM machines WHERE state != ? ORDER BY id`,
		state.StateDestroyed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.Machine
	for rows.Next() {
		var m state.Machine
		if err := scanMachine(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PutMachine writes a machine's row, and DELIBERATELY never writes host_id on
// an existing row.
//
// Ownership moves only through ClaimMachine. This is what makes the
// resurrected-owner case survivable: a host that was partitioned while its
// machine was rescued comes back with a stale row and keeps writing --
// heartbeats, state, build ids -- and none of those writes can take the
// machine back, because host_id is not among the columns they touch. Merges
// are per column, so a full-row write that re-asserted host_id would win on
// that column purely by being later.
//
// The `WHERE machines.host_id = ?` on the update path is the single-writer
// rule itself, enforced by the database rather than by review: a write aimed
// at another host's machine changes nothing and comes back as ErrNotOwner.
func (s *Store) PutMachine(ctx context.Context, m *state.Machine, opts ...state.WriteOption) error {
	auth := state.ResolveAuth(opts)

	guard := ` WHERE machines.host_id = ?`
	params := []any{
		m.ID, m.Name, m.HostID, m.State, m.KindKnobs, m.ImageRef, m.VCPUs, m.MemMiB,
		m.Domain, m.CustomDomain, m.AppPort, m.AgentPort, m.AgentTokenHash,
		m.MemBuildID, m.RootfsBuildID, m.TemplateMemBuildID, m.TemplateRootfsBuildID,
		m.VolumeID, m.ServiceID, m.ReleaseID, m.App, m.Slot,
		m.LastActivity, m.UpdatedAt,
	}
	if auth.NameAllocation || auth.DeadOwnerClaim != "" {
		guard = ""
	} else {
		params = append(params, s.hostID)
	}

	res, err := s.client.Exec(ctx, `
		INSERT INTO machines (`+machineCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, state=excluded.state,
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
			updated_at=excluded.updated_at`+guard,
		params...)
	if err != nil {
		return fmt.Errorf("state: put machine %q: %w", m.ID, err)
	}
	if res.RowsAffected == 0 && guard != "" {
		return fmt.Errorf("state: put machine %q: %w", m.ID, state.ErrNotOwner)
	}
	return nil
}

// ClaimMachine takes a machine from a host that has stopped heartbeating.
//
// The owner and the state move in ONE statement. Splitting them lets the merge
// interleave: the row ends up owned by the rescuer while still carrying
// whatever its dead owner last said about it, and the fleet has a machine that
// is running nowhere and claimed by someone.
//
// The claim is only legitimate while the owner is actually gone, so liveness
// is re-read here rather than trusted from the caller's tick -- the gap
// between a rescue loop deciding and writing is exactly where a host comes
// back.
func (s *Store) ClaimMachine(ctx context.Context, id, newHostID, newState string, opts ...state.WriteOption) error {
	auth := state.ResolveAuth(opts)
	if auth.DeadOwnerClaim == "" {
		return fmt.Errorf("state: claiming %q needs WithDeadOwnerClaim naming the "+
			"host it is taken from: %w", id, state.ErrNotOwner)
	}

	alive, err := s.hostIsLive(ctx, auth.DeadOwnerClaim)
	if err != nil {
		return err
	}
	if alive {
		return fmt.Errorf("state: refusing to claim %q from %s, which is still "+
			"heartbeating: %w", id, auth.DeadOwnerClaim, state.ErrNotOwner)
	}

	res, err := s.client.Exec(ctx,
		`UPDATE machines SET host_id = ?, state = ?, updated_at = ?
		 WHERE id = ? AND host_id = ?`,
		newHostID, newState, time.Now().Unix(), id, auth.DeadOwnerClaim)
	if err != nil {
		return fmt.Errorf("state: claim machine %q: %w", id, err)
	}
	if res.RowsAffected == 0 {
		// Someone else claimed it first, or it moved. Either way it is not
		// ours and the next tick re-hashes.
		return fmt.Errorf("state: claim machine %q: %w", id, state.ErrNotOwner)
	}
	return nil
}

// hostIsLive reports whether a host has heartbeated recently enough to count.
func (s *Store) hostIsLive(ctx context.Context, hostID string) (bool, error) {
	rows, err := s.client.Query(ctx, `SELECT last_seen FROM hosts WHERE id = ?`, hostID)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	if !rows.Next() {
		// A host with no row has never heartbeated: not live.
		return false, rows.Err()
	}
	var lastSeen int64
	if err := rows.Scan(&lastSeen); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return time.Since(time.Unix(lastSeen, 0)) < deadAfter, nil
}

// DeleteMachine tombstones a machine.
//
// Never an actual DELETE. A delete racing a concurrent update loses through
// the merge -- the update's newer per-column clocks resurrect the row -- and
// the fleet gets a machine nobody meant to keep. Marking it destroyed is a
// write like any other, so it merges predictably, and the reaper collects the
// row once no host can still be holding a stale view of it.
func (s *Store) DeleteMachine(ctx context.Context, id string) error {
	res, err := s.client.Exec(ctx,
		`UPDATE machines SET state = ?, updated_at = ? WHERE id = ? AND host_id = ?`,
		state.StateDestroyed, time.Now().Unix(), id, s.hostID)
	if err != nil {
		return fmt.Errorf("state: destroy machine %q: %w", id, err)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("state: destroy machine %q: %w", id, state.ErrNotOwner)
	}
	return nil
}

// TouchMachine records activity without touching anything else.
//
// Only the activity columns, for the reason this method exists at all: a
// read-modify-write of the whole row would carry every other column's value
// with it and clobber a concurrent lifecycle change.
func (s *Store) TouchMachine(ctx context.Context, id string, now int64) error {
	_, err := s.client.Exec(ctx,
		`UPDATE machines SET last_activity = ? WHERE id = ? AND host_id = ?`,
		now, id, s.hostID)
	if err != nil {
		return fmt.Errorf("state: touch machine %q: %w", id, err)
	}
	// A machine this host does not own is not an error here: activity is
	// recorded by whoever served the request, and the owner records its own.
	return nil
}

// PutHost writes this host's own row. It is the only row a host may write.
func (s *Store) PutHost(ctx context.Context, h *state.Host) error {
	if h.ID != s.hostID {
		return fmt.Errorf("state: host %s cannot write host %s's row: %w",
			s.hostID, h.ID, state.ErrNotOwner)
	}
	_, err := s.client.Exec(ctx, `
		INSERT INTO hosts (id, wg_addr, wg_pubkey, public_ip, cpu_free, mem_free_mib, last_seen)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			wg_addr=excluded.wg_addr, wg_pubkey=excluded.wg_pubkey,
			public_ip=excluded.public_ip, cpu_free=excluded.cpu_free,
			mem_free_mib=excluded.mem_free_mib, last_seen=excluded.last_seen`,
		h.ID, h.WGAddr, h.WGPubKey, h.PublicIP, h.CPUFree, h.MemFreeMiB, h.LastSeen)
	if err != nil {
		return fmt.Errorf("state: put host %q: %w", h.ID, err)
	}
	return nil
}

func (s *Store) ListHosts(ctx context.Context) ([]state.Host, error) {
	rows, err := s.client.Query(ctx,
		`SELECT id, wg_addr, wg_pubkey, public_ip, cpu_free, mem_free_mib, last_seen
		 FROM hosts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.Host
	for rows.Next() {
		var h state.Host
		if err := rows.Scan(&h.ID, &h.WGAddr, &h.WGPubKey, &h.PublicIP,
			&h.CPUFree, &h.MemFreeMiB, &h.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) PutCheckpoint(ctx context.Context, c *state.Checkpoint) error {
	_, err := s.client.Exec(ctx, `
		INSERT INTO checkpoints (id, machine_id, seq, comment, source_id,
			mem_build_id, rootfs_build_id, durable, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			comment=excluded.comment, mem_build_id=excluded.mem_build_id,
			rootfs_build_id=excluded.rootfs_build_id, durable=excluded.durable`,
		c.ID, c.MachineID, c.Seq, c.Comment, c.SourceID,
		c.MemBuildID, c.RootfsBuildID, boolToInt(c.Durable), c.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put checkpoint %q: %w", c.ID, err)
	}
	return nil
}

func (s *Store) ListCheckpoints(ctx context.Context, machineID string) ([]state.Checkpoint, error) {
	rows, err := s.client.Query(ctx, `
		SELECT id, machine_id, seq, comment, source_id, mem_build_id,
			rootfs_build_id, durable, created_at
		FROM checkpoints WHERE machine_id = ? ORDER BY seq`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.Checkpoint
	for rows.Next() {
		var (
			c       state.Checkpoint
			durable int
		)
		if err := rows.Scan(&c.ID, &c.MachineID, &c.Seq, &c.Comment, &c.SourceID,
			&c.MemBuildID, &c.RootfsBuildID, &durable, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Durable = durable != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCheckpoint(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM checkpoints WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete checkpoint %q: %w", id, err)
	}
	return nil
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (*state.APIKey, error) {
	rows, err := s.client.Query(ctx,
		`SELECT hash, org_id, scopes, created_at FROM api_keys WHERE hash = ?`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: api key: %w", state.ErrNotFound)
	}
	var k state.APIKey
	if err := rows.Scan(&k.Hash, &k.OrgID, &k.Scopes, &k.CreatedAt); err != nil {
		return nil, err
	}
	return &k, rows.Err()
}

func (s *Store) PutAPIKey(ctx context.Context, k *state.APIKey) error {
	_, err := s.client.Exec(ctx, `
		INSERT INTO api_keys (hash, org_id, scopes, created_at) VALUES (?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET org_id=excluded.org_id, scopes=excluded.scopes`,
		k.Hash, k.OrgID, k.Scopes, k.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put api key: %w", err)
	}
	return nil
}

func (s *Store) ListAPIKeys(ctx context.Context, orgID string) ([]state.APIKey, error) {
	rows, err := s.client.Query(ctx,
		`SELECT hash, org_id, scopes, created_at FROM api_keys
		 WHERE org_id = ? ORDER BY created_at DESC, hash`, orgID)
	if err != nil {
		return nil, fmt.Errorf("state: list api keys: %w", err)
	}
	defer rows.Close()

	var out []state.APIKey
	for rows.Next() {
		var k state.APIKey
		if err := rows.Scan(&k.Hash, &k.OrgID, &k.Scopes, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: scan api key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PutTenancy records which org owns an object.
//
// Unguarded by single-writer, and DO NOTHING rather than DO UPDATE. Those two
// go together: any host may write this row precisely because nothing can ever
// change a value it already holds, so two hosts racing cannot produce a merge
// neither of them wrote. Turn this into DO UPDATE and "any host may write it"
// stops being true.
func (s *Store) PutTenancy(ctx context.Context, t *state.Tenancy) error {
	_, err := s.client.Exec(ctx, `
		INSERT INTO tenancy (id, org_id, kind, created_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		t.ID, t.OrgID, t.Kind, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put tenancy %q: %w", t.ID, err)
	}
	return nil
}

func (s *Store) GetTenancy(ctx context.Context, id string) (*state.Tenancy, error) {
	rows, err := s.client.Query(ctx,
		`SELECT id, org_id, kind, created_at FROM tenancy WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: tenancy %q: %w", id, state.ErrNotFound)
	}
	var t state.Tenancy
	if err := rows.Scan(&t.ID, &t.OrgID, &t.Kind, &t.CreatedAt); err != nil {
		return nil, err
	}
	return &t, rows.Err()
}

func (s *Store) ListTenancy(ctx context.Context) ([]state.Tenancy, error) {
	rows, err := s.client.Query(ctx,
		`SELECT id, org_id, kind, created_at FROM tenancy ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list tenancy: %w", err)
	}
	defer rows.Close()

	var out []state.Tenancy
	for rows.Next() {
		var t state.Tenancy
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Kind, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: scan tenancy: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutRevocation kills a key by ADDING a row. Never by deleting the api_keys
// one: a delete racing a replica that still carries the insert loses the race
// through the merge, and the revoked credential authenticates again.
func (s *Store) PutRevocation(ctx context.Context, rv *state.Revocation) error {
	_, err := s.client.Exec(ctx, `
		INSERT INTO api_key_revocations (hash, revoked_at) VALUES (?,?)
		ON CONFLICT(hash) DO NOTHING`, rv.Hash, rv.RevokedAt)
	if err != nil {
		return fmt.Errorf("state: put revocation: %w", err)
	}
	return nil
}

func (s *Store) GetRevocation(ctx context.Context, hash string) (*state.Revocation, error) {
	rows, err := s.client.Query(ctx,
		`SELECT hash, revoked_at FROM api_key_revocations WHERE hash = ?`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: revocation: %w", state.ErrNotFound)
	}
	var rv state.Revocation
	if err := rows.Scan(&rv.Hash, &rv.RevokedAt); err != nil {
		return nil, err
	}
	return &rv, rows.Err()
}

func (s *Store) IsRevoked(ctx context.Context, hash string) (bool, error) {
	rows, err := s.client.Query(ctx,
		`SELECT 1 FROM api_key_revocations WHERE hash = ?`, hash)
	if err != nil {
		return false, fmt.Errorf("state: check revocation: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return false, rows.Err()
	}
	return true, rows.Err()
}

const quotaCols = `org_id, max_machines, max_vcpus, max_mem_mib, max_volume_gib, max_builds, updated_at`

func (s *Store) GetQuota(ctx context.Context, orgID string) (*state.Quota, error) {
	rows, err := s.client.Query(ctx,
		`SELECT `+quotaCols+` FROM org_quotas WHERE org_id = ?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: quota %q: %w", orgID, state.ErrNotFound)
	}
	var q state.Quota
	if err := rows.Scan(&q.OrgID, &q.MaxMachines, &q.MaxVCPUs, &q.MaxMemMiB,
		&q.MaxVolumeGiB, &q.MaxBuilds, &q.UpdatedAt); err != nil {
		return nil, err
	}
	return &q, rows.Err()
}

// PutQuota updates in place, unlike the two tables above. One logical writer
// -- an admin request -- so last-write-wins between two admins editing the
// same org is the semantics rather than a hazard.
func (s *Store) PutQuota(ctx context.Context, q *state.Quota) error {
	_, err := s.client.Exec(ctx, `
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
	return nil
}

func (s *Store) GetTemplate(ctx context.Context, id string) (*state.Template, error) {
	rows, err := s.client.Query(ctx,
		`SELECT id, descriptor, created_at FROM templates WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: template %q: %w", id, state.ErrNotFound)
	}
	var t state.Template
	var descriptor string
	if err := rows.Scan(&t.ID, &descriptor, &t.CreatedAt); err != nil {
		return nil, err
	}
	if err := state.UnmarshalDescriptor(&t, descriptor); err != nil {
		return nil, err
	}
	return &t, rows.Err()
}

// PutTemplate publishes the fleet's golden template.
//
// Unguarded by single-writer on purpose: the template belongs to the fleet
// rather than to a host, and the row is written once by whichever host finds
// none. A concurrent second writer costs a wasted build and nothing else --
// but only because the parts that must agree travel in one column, so a merge
// cannot pair one host's memory build with another's disk build, and because
// each build writes its vmstate under its own key rather than over the
// other's.
func (s *Store) PutTemplate(ctx context.Context, t *state.Template) error {
	descriptor, err := state.MarshalDescriptor(t)
	if err != nil {
		return err
	}
	_, err = s.client.Exec(ctx, `
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

const volumeCols = `id, name, machine_id, size_mib, s3_prefix, mount_path, host_id, created_at`

func (s *Store) GetVolume(ctx context.Context, id string) (*state.Volume, error) {
	rows, err := s.client.Query(ctx, `SELECT `+volumeCols+` FROM volumes WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: volume %q: %w", id, state.ErrNotFound)
	}
	var v state.Volume
	if err := rows.Scan(&v.ID, &v.Name, &v.MachineID, &v.SizeMiB, &v.S3Prefix,
		&v.MountPath, &v.HostID, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, rows.Err()
}

func (s *Store) ListVolumes(ctx context.Context) ([]state.Volume, error) {
	rows, err := s.client.Query(ctx, `SELECT `+volumeCols+` FROM volumes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.Volume
	for rows.Next() {
		var v state.Volume
		if err := rows.Scan(&v.ID, &v.Name, &v.MachineID, &v.SizeMiB, &v.S3Prefix,
			&v.MountPath, &v.HostID, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PutVolume writes a volume's row, guarded by the same `WHERE host_id = ?`
// that guards a machine.
//
// The guard is doing more work here than it does for a machine. A machine two
// hosts both believe they own is a state bug that surfaces as a duplicate
// process; a VOLUME two hosts both believe they own is two juicefs mounts
// against one SQLite metadata database, which is how the volume stops
// existing. The single legitimate exception is a rescuer taking it from an
// owner that has stopped heartbeating, and the liveness of that owner is
// re-read here rather than trusted from the caller -- the gap between deciding
// to rescue and writing is exactly where a host comes back.
func (s *Store) PutVolume(ctx context.Context, v *state.Volume, opts ...state.WriteOption) error {
	auth := state.ResolveAuth(opts)

	// An unowned row is claimable by anyone: releaseVolume clears host_id, and
	// a volume nobody owns is by definition mounted nowhere. Without this the
	// guard admits only the current owner, so a released volume could never be
	// picked up again -- not by another host, and not even by this one.
	guard := ` WHERE volumes.host_id = ? OR volumes.host_id = ''`
	params := []any{v.ID, v.Name, v.MachineID, v.SizeMiB, v.S3Prefix,
		v.MountPath, v.HostID, v.CreatedAt}

	if auth.DeadOwnerClaim != "" {
		alive, err := s.hostIsLive(ctx, auth.DeadOwnerClaim)
		if err != nil {
			return err
		}
		if alive {
			return fmt.Errorf("state: refusing to take volume %q from %s, which is "+
				"still heartbeating and may still have it mounted: %w",
				v.ID, auth.DeadOwnerClaim, state.ErrNotOwner)
		}
		params = append(params, auth.DeadOwnerClaim)
	} else {
		params = append(params, s.hostID)
	}

	res, err := s.client.Exec(ctx, `
		INSERT INTO volumes (`+volumeCols+`)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, machine_id=excluded.machine_id,
			size_mib=excluded.size_mib, s3_prefix=excluded.s3_prefix,
			mount_path=excluded.mount_path, host_id=excluded.host_id,
			created_at=excluded.created_at`+guard,
		params...)
	if err != nil {
		return fmt.Errorf("state: put volume %q: %w", v.ID, err)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("state: put volume %q: %w", v.ID, state.ErrNotOwner)
	}
	return nil
}

// Close releases the store. The agent is a separate process with its own
// lifetime, so there is nothing here to shut down.
func (s *Store) Close() error { return nil }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Store satisfies the interface every other package depends on.
var _ state.Store = (*Store)(nil)

// serviceCols is the column list, in the order rows are scanned.
const serviceCols = `id, name, app, release_id, replicas, health, env, env_sealed,
	domain, custom_domain, repo, branch, autodeploy, created_at`

// DeleteService removes a service row.
//
// A real delete rather than a tombstone column: unlike a machine, nothing
// routes to a service by id after it is gone, and the row carries the sealed
// environment -- so the point is that it stops being replicated at all.
func (s *Store) DeleteService(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM services WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete service %q: %w", id, err)
	}
	return nil
}

func (s *Store) GetService(ctx context.Context, id string) (*state.Service, error) {
	rows, err := s.client.Query(ctx, `SELECT `+serviceCols+` FROM services WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: service %q: %w", id, state.ErrNotFound)
	}
	// SQLite has no boolean, so the column is an INTEGER and corrosion hands it
	// back as a JSON number. Scanning it straight into a bool fails at the
	// decoder with a message about types rather than about the column, and it
	// fails on every read of the table -- which reached a caller as a create
	// that could not deliver an environment.
	var autodeploy int
	var svc state.Service
	if err := rows.Scan(&svc.ID, &svc.Name, &svc.App, &svc.ReleaseID, &svc.Replicas,
		&svc.Health, &svc.Env, &svc.EnvSealed, &svc.Domain, &svc.CustomDomain,
		&svc.Repo, &svc.Branch, &autodeploy, &svc.CreatedAt); err != nil {
		return nil, err
	}
	svc.Autodeploy = autodeploy != 0
	return &svc, rows.Err()
}

// PutService writes a service row.
//
// There is no host_id on a service to guard the write with, the way PutMachine
// guards on ownership -- a service names machines, not a host. The rule is
// kept where it can be: the only caller is the create path, which runs on the
// host that is about to own the machine.
// PutService writes a service row, and refuses if this host is not its writer.
//
// PutMachine can enforce single-writer in SQL because a machine row names its
// host; a service row names machines, so there is no column to guard on. The
// guard is instead the deterministic arbiter every host computes identically
// from the live set (state.OwnerFor — the same function that decides which
// host rescues a machine). Non-arbiter hosts forward the API call over the
// mesh, so by the time this runs on the arbiter, self IS the arbiter and no
// forwarding mark is needed.
//
// This matters because 5c gives services more writers than the create path
// that 5b left them with: a deploy flips release_id, an autoscaler writes
// replicas. Two hosts writing one row under last-write-wins does not error,
// does not conflict, and silently keeps half of each write -- the exact
// failure mode fly designed Corrosion's usage around ("workers own their own
// state, so updates from different workers almost never conflict",
// fly.io/blog/corrosion).
//
// The residual window is arbiter flicker: while two hosts disagree about
// liveness they can each believe they are the writer. That window is bounded
// by heartbeat divergence and is the same exposure Phase 4 already accepted
// for rescue. Deliberately not closed with a distributed lock -- there is no
// coordinator here by design, and a fake one would be worse than a documented
// window. The deploy path additionally CASes on the value it replaces
// (CASServiceRelease) so the one race that corrupts is refused outright.
func (s *Store) PutService(ctx context.Context, svc *state.Service) error {
	if err := s.assertServiceWriter(ctx, svc.ID); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
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
		svc.Branch, boolToInt(svc.Autodeploy), svc.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put service %q: %w", svc.ID, err)
	}
	return nil
}

// assertServiceWriter refuses a service write on a host that is not the
// arbiter for it.
//
// A fleet with no live hosts in its own table is a host that has not finished
// starting; allow the write rather than deadlock a single-box bring-up, since
// with no peers there is no one to race.
func (s *Store) assertServiceWriter(ctx context.Context, serviceID string) error {
	live, err := s.liveHosts(ctx)
	if err != nil {
		return fmt.Errorf("state: service writer check for %q: %w", serviceID, err)
	}
	owner, ok := state.OwnerFor(serviceID, live)
	if !ok || owner == s.hostID {
		return nil
	}
	return fmt.Errorf("state: service %q is written by %s, not this host: %w",
		serviceID, owner, state.ErrNotOwner)
}

// liveHosts is the membership the arbiter is computed from.
//
// Deliberately the same deadAfter cutoff hostIsLive uses, so a host that is
// too dead to have its machines claimed is also too dead to be a service
// writer. Two different liveness views would put the two mechanisms out of
// step during exactly the partition where that hurts most.
func (s *Store) liveHosts(ctx context.Context) ([]state.Host, error) {
	all, err := s.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]state.Host, 0, len(all))
	for _, h := range all {
		if time.Since(time.Unix(h.LastSeen, 0)) < deadAfter {
			out = append(out, h)
		}
	}
	return out, nil
}

// CASServiceRelease flips a service to a new release only if it still carries
// the release the caller last saw.
//
// The deploy path's one genuinely corrupting race: two deploys interleaving
// leave a service pointing at one release while the other's machines are the
// ones actually running. The arbiter makes this rare; the compare-and-swap
// makes it impossible to lose silently. Deliberately a targeted UPDATE rather
// than a whole-row write with a version column -- services already carries
// rows, and adding a column to a populated Corrosion table is the fleet-wide
// backfill that took fly down twice.
func (s *Store) CASServiceRelease(ctx context.Context, id, from, to string) error {
	if err := s.assertServiceWriter(ctx, id); err != nil {
		return err
	}
	res, err := s.client.Exec(ctx,
		`UPDATE services SET release_id = ? WHERE id = ? AND release_id = ?`,
		to, id, from)
	if err != nil {
		return fmt.Errorf("state: flip service %q to release %q: %w", id, to, err)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("state: service %q no longer carries release %q: %w",
			id, from, state.ErrNotOwner)
	}
	return nil
}

// releaseCols is the column list, in the order rows are scanned.
const releaseCols = `id, service_id, rootfs_build_id, mem_build_id, healthy, created_at`

func (s *Store) GetRelease(ctx context.Context, id string) (*state.Release, error) {
	rows, err := s.client.Query(ctx, `SELECT `+releaseCols+` FROM releases WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("state: get release %q: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: release %q: %w", id, state.ErrNotFound)
	}
	r, err := scanRelease(rows)
	if err != nil {
		return nil, fmt.Errorf("state: scan release %q: %w", id, err)
	}
	return r, rows.Err()
}

func (s *Store) PutRelease(ctx context.Context, r *state.Release) error {
	// A release inherits its service's writer rather than having an arbiter of
	// its own: it is only ever written by the host running that service's
	// deploy, which the service arbiter already selected.
	if err := s.assertServiceWriter(ctx, r.ServiceID); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
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

func (s *Store) ReleasesFor(ctx context.Context, serviceID string) ([]state.Release, error) {
	rows, err := s.client.Query(ctx, `SELECT `+releaseCols+
		` FROM releases WHERE service_id = ? ORDER BY created_at DESC`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("state: releases for %q: %w", serviceID, err)
	}
	defer rows.Close()

	var out []state.Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan release: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) ListServices(ctx context.Context) ([]state.Service, error) {
	rows, err := s.client.Query(ctx, `SELECT `+serviceCols+` FROM services`)
	if err != nil {
		return nil, fmt.Errorf("state: list services: %w", err)
	}
	defer rows.Close()

	var out []state.Service
	for rows.Next() {
		// Same integer-bool handling as GetService: corrosion returns a JSON
		// number for autodeploy, never a bool.
		var autodeploy int
		var svc state.Service
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.App, &svc.ReleaseID, &svc.Replicas,
			&svc.Health, &svc.Env, &svc.EnvSealed, &svc.Domain, &svc.CustomDomain,
			&svc.Repo, &svc.Branch, &autodeploy, &svc.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: scan service: %w", err)
		}
		svc.Autodeploy = autodeploy != 0
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) ListServiceNames(ctx context.Context) ([]state.Service, error) {
	rows, err := s.client.Query(ctx, `SELECT id, name, app FROM services`)
	if err != nil {
		return nil, fmt.Errorf("state: list service names: %w", err)
	}
	defer rows.Close()

	var out []state.Service
	for rows.Next() {
		var svc state.Service
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.App); err != nil {
			return nil, fmt.Errorf("state: scan service name: %w", err)
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// scanRelease reads one release row. healthy arrives as a JSON number from
// corrosion, never a bool -- SQLite has no bool type.
func scanRelease(rows *Rows) (*state.Release, error) {
	var r state.Release
	var healthy int
	if err := rows.Scan(&r.ID, &r.ServiceID, &r.RootfsBuildID, &r.MemBuildID,
		&healthy, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.Healthy = healthy != 0
	return &r, nil
}

const domainCols = `hostname, service_id, verified_at, created_at`

func (s *Store) GetDomain(ctx context.Context, hostname string) (*state.Domain, error) {
	rows, err := s.client.Query(ctx, `SELECT `+domainCols+` FROM domains WHERE hostname = ?`, hostname)
	if err != nil {
		return nil, fmt.Errorf("state: get domain %q: %w", hostname, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: domain %q: %w", hostname, state.ErrNotFound)
	}
	var d state.Domain
	if err := rows.Scan(&d.Hostname, &d.ServiceID, &d.VerifiedAt, &d.CreatedAt); err != nil {
		return nil, err
	}
	return &d, rows.Err()
}

// PutDomain writes a custom hostname.
//
// Guarded by the service's arbiter for the same reason PutService is: a domain
// row names a service rather than a host, so there is no column to enforce
// single-writer on, and two hosts pointing one hostname at different services
// would merge under last-write-wins into whichever wrote last.
func (s *Store) PutDomain(ctx context.Context, d *state.Domain) error {
	if err := s.assertServiceWriter(ctx, d.ServiceID); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
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

func (s *Store) DeleteDomain(ctx context.Context, hostname string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM domains WHERE hostname = ?`, hostname); err != nil {
		return fmt.Errorf("state: delete domain %q: %w", hostname, err)
	}
	return nil
}

func (s *Store) ListDomains(ctx context.Context) ([]state.Domain, error) {
	rows, err := s.client.Query(ctx, `SELECT `+domainCols+` FROM domains`)
	if err != nil {
		return nil, fmt.Errorf("state: list domains: %w", err)
	}
	defer rows.Close()
	var out []state.Domain
	for rows.Next() {
		var d state.Domain
		if err := rows.Scan(&d.Hostname, &d.ServiceID, &d.VerifiedAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Version is the sum of the replica's version vector, one db_version per
// actor: the number of changes this replica has applied from every host, so
// two hosts' values say how far apart they are. The scalar crsql_db_version()
// is the local write clock and does not compare across hosts -- two replicas
// that hold identical data but wrote a different share of it carry different
// values, so a comparison of those says nothing at all.
//
// The table is cr-sqlite's, not this schema's: one row per actor, keyed by
// site_id. Its shape is pinned in the test as crsqlDBVersionsDDL, taken from a
// running corrosion agent at the version scripts/host-bootstrap.sh installs.
// An error here reads as version 0 at the caller rather than a guess -- see
// /v1/health, which logs it and stays up.
func (s *Store) Version(ctx context.Context) (int64, error) {
	rows, err := s.client.Query(ctx, `SELECT COALESCE(SUM(db_version), 0) FROM crsql_db_versions`)
	if err != nil {
		return 0, fmt.Errorf("state: store version: %w", err)
	}
	defer rows.Close()
	var v int64
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("state: store version: %w", err)
		}
	}
	return v, rows.Err()
}
