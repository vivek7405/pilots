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
	volume_id, service_id, release_id,
	last_activity, updated_at`

func scanMachine(rows *Rows, m *state.Machine) error {
	return rows.Scan(&m.ID, &m.Name, &m.HostID, &m.State, &m.KindKnobs, &m.ImageRef,
		&m.VCPUs, &m.MemMiB, &m.Domain, &m.CustomDomain, &m.AppPort, &m.AgentPort,
		&m.AgentTokenHash, &m.MemBuildID, &m.RootfsBuildID,
		&m.TemplateMemBuildID, &m.TemplateRootfsBuildID, &m.VolumeID,
		&m.ServiceID, &m.ReleaseID, &m.LastActivity, &m.UpdatedAt)
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
		m.VolumeID, m.ServiceID, m.ReleaseID,
		m.LastActivity, m.UpdatedAt,
	}
	if auth.NameAllocation || auth.DeadOwnerClaim != "" {
		guard = ""
	} else {
		params = append(params, s.hostID)
	}

	res, err := s.client.Exec(ctx, `
		INSERT INTO machines (`+machineCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
			release_id=excluded.release_id, last_activity=excluded.last_activity,
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
