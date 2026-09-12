package corrosion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

	// A planned handoff: a LIVE host offered this machine, so liveness is not
	// the test. The OFFER is, and every part of it is checked here rather than
	// trusted from the caller -- this is the one path on which a running
	// fleet's machine changes owner, so it is the one place a mistake would
	// give two hosts one machine.
	if auth.HandoffID != "" {
		return s.claimByHandoff(ctx, id, newHostID, newState, auth.HandoffID)
	}

	if auth.DeadOwnerClaim == "" {
		return fmt.Errorf("state: claiming %q needs WithDeadOwnerClaim naming the "+
			"host it is taken from, or WithHandoff naming an offer: %w", id, state.ErrNotOwner)
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

// claimByHandoff takes a machine a live host offered.
//
// Five checks, and every one of them is load-bearing:
//
//  1. The offer exists. Without it there is nothing authorising this at all.
//  2. It names THIS host. Otherwise any host could take a machine offered to
//     somebody else by quoting the id.
//  3. Its from_host is the machine's CURRENT owner. An offer made before the
//     machine moved is stale, and honouring it would take the machine from
//     whoever holds it now.
//  4. It is the machine's NEWEST offer. A source that gave up on one target
//     and offered the machine to another must not have the first target
//     arrive late and take it.
//  5. The machine is not running. The source suspends before it offers, so a
//     running row means the offer has not been acted on by its writer yet --
//     or the machine came back -- and taking it would leave two Firecrackers
//     for one id.
//
// Gossip may deliver the offer before the suspended write that should precede
// it; check 5 absorbs that by refusing until the machine is actually down, and
// the caller retries.
func (s *Store) claimByHandoff(ctx context.Context, id, newHostID, newState, handoffID string) error {
	rows, err := s.client.Query(ctx, `
		SELECT machine_id, from_host, to_host, seq FROM machine_handoffs WHERE id = ?`, handoffID)
	if err != nil {
		return err
	}
	var h state.Handoff
	found := rows.Next()
	if found {
		if err := rows.Scan(&h.MachineID, &h.FromHost, &h.ToHost, &h.Seq); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if !found {
		return fmt.Errorf("state: no handoff %q offers %q: %w", handoffID, id, state.ErrNotOwner)
	}
	if h.MachineID != id {
		return fmt.Errorf("state: handoff %q offers %q, not %q: %w",
			handoffID, h.MachineID, id, state.ErrNotOwner)
	}
	if h.ToHost != newHostID {
		return fmt.Errorf("state: handoff %q offers %q to %s, not to %s: %w",
			handoffID, id, h.ToHost, newHostID, state.ErrNotOwner)
	}

	newest, err := s.NewestHandoff(ctx, id)
	if err != nil {
		return err
	}
	if newest.ID != handoffID {
		return fmt.Errorf("state: handoff %q for %q is superseded by %q: %w",
			handoffID, id, newest.ID, state.ErrNotOwner)
	}

	// The UPDATE carries the rest: from_host must still be the owner, and the
	// machine must not be running. Both in the WHERE clause rather than read
	// first and checked, so there is no window between the check and the write
	// for either to change.
	res, err := s.client.Exec(ctx,
		`UPDATE machines SET host_id = ?, state = ?, updated_at = ?
		 WHERE id = ? AND host_id = ? AND state != ?`,
		newHostID, newState, time.Now().Unix(), id, h.FromHost, state.StateRunning)
	if err != nil {
		return fmt.Errorf("state: claim machine %q by handoff: %w", id, err)
	}
	if res.RowsAffected == 0 {
		// Either it is no longer on the offering host, or it is running. The
		// caller retries: on a drain the source is about to suspend it, and
		// gossip may simply not have caught up.
		return fmt.Errorf("state: %q is not where handoff %q said, or is still "+
			"running: %w", id, handoffID, state.ErrNotOwner)
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
		`SELECT h.id, h.wg_addr, h.wg_pubkey, h.public_ip, h.cpu_free, h.mem_free_mib,
			h.last_seen, COALESCE(c.vendor, '')
		 FROM hosts h LEFT JOIN host_cpu c ON c.host_id = h.id
		 ORDER BY h.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.Host
	for rows.Next() {
		var h state.Host
		if err := rows.Scan(&h.ID, &h.WGAddr, &h.WGPubKey, &h.PublicIP,
			&h.CPUFree, &h.MemFreeMiB, &h.LastSeen, &h.Vendor); err != nil {
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

// PutAPIKeyLimits records what a restricted key may do, by ADDING a row and
// never changing one: the limits a human approved when the key was minted are
// the only limits it ever has, and a write that could widen them later would
// undo the consent that produced them.
func (s *Store) PutAPIKeyLimits(ctx context.Context, l *state.APIKeyLimits) error {
	_, err := s.client.Exec(ctx, `
		INSERT INTO api_key_limits (hash, name_prefix, max_machines, expires_at, created_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(hash) DO NOTHING`,
		l.Hash, l.NamePrefix, l.MaxMachines, l.ExpiresAt, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put api key limits: %w", err)
	}
	return nil
}

func (s *Store) GetAPIKeyLimits(ctx context.Context, hash string) (*state.APIKeyLimits, error) {
	rows, err := s.client.Query(ctx,
		`SELECT hash, name_prefix, max_machines, expires_at, created_at FROM api_key_limits WHERE hash = ?`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: api key limits: %w", state.ErrNotFound)
	}
	var l state.APIKeyLimits
	if err := rows.Scan(&l.Hash, &l.NamePrefix, &l.MaxMachines, &l.ExpiresAt, &l.CreatedAt); err != nil {
		return nil, err
	}
	return &l, rows.Err()
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

const repoLinkCols = `id, org_id, repo, connected_at`

// PutRepoLink connects a repository to an org.
//
// Unguarded by single-writer and DO NOTHING rather than DO UPDATE, exactly as
// PutTenancy is, and for the same reason: any host may write this row
// precisely because nothing can ever change a value it already holds, so two
// hosts racing cannot produce a merge neither of them wrote. Turn this into DO
// UPDATE and "any host may write it" stops being true.
func (s *Store) PutRepoLink(ctx context.Context, l *state.RepoLink) error {
	_, err := s.client.Exec(ctx, `
		INSERT INTO repo_links (`+repoLinkCols+`) VALUES (?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		state.RepoLinkID(l.OrgID, l.Repo), l.OrgID, state.NormalizeRepo(l.Repo), l.ConnectedAt)
	if err != nil {
		return fmt.Errorf("state: put repo link %q: %w", l.Repo, err)
	}
	return nil
}

// GetRepoLink is a point read against the LOCAL replica, on the request path
// of every {repo, ref} build and plan. It must never grow a network hop.
func (s *Store) GetRepoLink(ctx context.Context, orgID, repo string) (*state.RepoLink, error) {
	rows, err := s.client.Query(ctx,
		`SELECT `+repoLinkCols+` FROM repo_links WHERE id = ?`, state.RepoLinkID(orgID, repo))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state: repo link %q: %w", repo, state.ErrNotFound)
	}
	var l state.RepoLink
	if err := rows.Scan(&l.ID, &l.OrgID, &l.Repo, &l.ConnectedAt); err != nil {
		return nil, err
	}
	return &l, rows.Err()
}

func (s *Store) ListRepoLinks(ctx context.Context, orgID string) ([]state.RepoLink, error) {
	var (
		rows *Rows
		err  error
	)
	if orgID == "" {
		rows, err = s.client.Query(ctx, `SELECT `+repoLinkCols+` FROM repo_links ORDER BY id`)
	} else {
		rows, err = s.client.Query(ctx,
			`SELECT `+repoLinkCols+` FROM repo_links WHERE org_id = ? ORDER BY id`, orgID)
	}
	if err != nil {
		return nil, fmt.Errorf("state: list repo links: %w", err)
	}
	defer rows.Close()

	var out []state.RepoLink
	for rows.Next() {
		var l state.RepoLink
		if err := rows.Scan(&l.ID, &l.OrgID, &l.Repo, &l.ConnectedAt); err != nil {
			return nil, fmt.Errorf("state: scan repo link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The dual read for the snapshot limit, which lives in its own table
	// because org_quotas has rows and a column add there is the cr-sqlite
	// backfill rule 6 forbids. An org with no row here is every org that
	// predates the table: it reads zero, which quota.Check takes as "use the
	// default" rather than as "refuse everything".
	snapRows, err := s.client.Query(ctx,
		`SELECT max_snapshot_gib FROM org_snapshot_quotas WHERE org_id = ?`, orgID)
	if err != nil {
		return &q, nil
	}
	defer snapRows.Close()
	if snapRows.Next() {
		_ = snapRows.Scan(&q.MaxSnapshotGiB)
	}
	return &q, nil
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
	if _, err := s.client.Exec(ctx, `
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

// PutHostCPU writes what THIS host says about its own CPU, and nothing else.
// Same rule as PutHost, for the same reason: one writer per row means the
// merge has nothing to corrupt.
func (s *Store) PutHostCPU(ctx context.Context, h *state.HostCPU) error {
	if h.HostID != s.hostID {
		return fmt.Errorf("state: host %s cannot write host %s's cpu row: %w",
			s.hostID, h.HostID, state.ErrNotOwner)
	}
	_, err := s.client.Exec(ctx, `
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

// PutHostCapacity records what this host can still hold.
//
// Only the host it names may write it, the same rule host_cpu follows. A host
// asserting another host's free memory would be asserting a fact it cannot
// observe, and placement would then send machines somewhere on the strength of
// it.
func (s *Store) PutHostCapacity(ctx context.Context, c *state.HostCapacity) error {
	if c.HostID != s.hostID {
		return fmt.Errorf("state: host %s cannot write host %s's capacity row: %w",
			s.hostID, c.HostID, state.ErrNotOwner)
	}
	draining := 0
	if c.Draining {
		draining = 1
	}
	_, err := s.client.Exec(ctx, `
		INSERT INTO host_capacity (host_id, mem_free_mib, mem_reclaimable_mib,
			cpu_count, vcpus_running, draining, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			mem_free_mib=excluded.mem_free_mib,
			mem_reclaimable_mib=excluded.mem_reclaimable_mib,
			cpu_count=excluded.cpu_count, vcpus_running=excluded.vcpus_running,
			draining=excluded.draining, updated_at=excluded.updated_at`,
		c.HostID, c.MemFreeMiB, c.MemReclaimableMiB, c.CPUCount, c.VCPUsRunning,
		draining, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put host capacity %q: %w", c.HostID, err)
	}
	return nil
}

func (s *Store) ListHostCapacity(ctx context.Context) ([]state.HostCapacity, error) {
	rows, err := s.client.Query(ctx, `
		SELECT host_id, mem_free_mib, mem_reclaimable_mib, cpu_count,
		       vcpus_running, draining, updated_at
		FROM host_capacity ORDER BY host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.HostCapacity
	for rows.Next() {
		var c state.HostCapacity
		var draining int
		if err := rows.Scan(&c.HostID, &c.MemFreeMiB, &c.MemReclaimableMiB,
			&c.CPUCount, &c.VCPUsRunning, &draining, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Draining = draining != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutHostBuilds records which builds this host has cached.
//
// The caller writes it only when the set changed, because this row is gossiped
// in full on every write and the cache changes far more often than placement
// needs to know about.
// PutVolumePolicy records a volume's snapshot schedule.
//
// Guarded like the volume row itself: only the host that MOUNTS the volume may
// write it, because that host is the only one that can act on the schedule. A
// policy written by a host that does not hold the volume would be a schedule
// nobody fires.
func (s *Store) PutVolumePolicy(ctx context.Context, p *state.VolumePolicy) error {
	if err := s.assertVolumeOwner(ctx, p.VolumeID); err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx, `
		INSERT INTO volume_policies (volume_id, cron, keep_daily, keep_weekly, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(volume_id) DO UPDATE SET
			cron=excluded.cron, keep_daily=excluded.keep_daily,
			keep_weekly=excluded.keep_weekly, updated_at=excluded.updated_at`,
		p.VolumeID, p.Cron, p.KeepDaily, p.KeepWeekly, p.UpdatedAt); err != nil {
		return fmt.Errorf("state: put volume policy %q: %w", p.VolumeID, err)
	}
	return nil
}

// assertVolumeOwner refuses a write about a volume this host does not hold.
//
// A volume with NO host is claimable: the write is part of creating it, or of
// taking one nobody has mounted. What is refused is writing about a volume
// another live host is using.
func (s *Store) assertVolumeOwner(ctx context.Context, volumeID string) error {
	v, err := s.GetVolume(ctx, volumeID)
	if err != nil {
		return err
	}
	if v.HostID != "" && v.HostID != s.hostID {
		return fmt.Errorf("state: host %s does not mount volume %s (%s does): %w",
			s.hostID, volumeID, v.HostID, state.ErrNotOwner)
	}
	return nil
}

func (s *Store) GetVolumePolicy(ctx context.Context, volumeID string) (*state.VolumePolicy, error) {
	rows, err := s.client.Query(ctx, `
		SELECT volume_id, cron, keep_daily, keep_weekly, updated_at
		FROM volume_policies WHERE volume_id = ?`, volumeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var p state.VolumePolicy
	if err := rows.Scan(&p.VolumeID, &p.Cron, &p.KeepDaily, &p.KeepWeekly, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) ListVolumePolicies(ctx context.Context) ([]state.VolumePolicy, error) {
	rows, err := s.client.Query(ctx, `
		SELECT volume_id, cron, keep_daily, keep_weekly, updated_at
		FROM volume_policies ORDER BY volume_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []state.VolumePolicy
	for rows.Next() {
		var p state.VolumePolicy
		if err := rows.Scan(&p.VolumeID, &p.Cron, &p.KeepDaily, &p.KeepWeekly, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteVolumePolicy(ctx context.Context, volumeID string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM volume_policies WHERE volume_id = ?`, volumeID); err != nil {
		return fmt.Errorf("state: delete volume policy %q: %w", volumeID, err)
	}
	return nil
}

// PutLineage records where a forked machine came from.
//
// Guarded like machine_labels: only the host that writes the FORK's machine
// row may write its lineage, because the row describes that machine. Write-once
// on top of that -- an upsert would let a later write change which builds are
// pinned, and the pinning is the only thing keeping a live fork's memory image
// from being discarded by its parent.
func (s *Store) PutLineage(ctx context.Context, l *state.Lineage) error {
	if err := s.assertMachineOwner(ctx, l.ID, state.WriteAuth{}); err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx, `
		INSERT INTO machine_lineage (id, parent_id, checkpoint_id, mem_build_id,
			rootfs_build_id, volume_snapshot, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.ParentID, l.CheckpointID, l.MemBuildID, l.RootfsBuildID,
		l.VolumeSnapshot, l.CreatedAt); err != nil {
		return fmt.Errorf("state: put lineage %q: %w", l.ID, err)
	}
	return nil
}

func (s *Store) GetLineage(ctx context.Context, machineID string) (*state.Lineage, error) {
	rows, err := s.client.Query(ctx, `
		SELECT id, parent_id, checkpoint_id, mem_build_id, rootfs_build_id,
		       volume_snapshot, created_at
		FROM machine_lineage WHERE id = ?`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var l state.Lineage
	if err := rows.Scan(&l.ID, &l.ParentID, &l.CheckpointID, &l.MemBuildID,
		&l.RootfsBuildID, &l.VolumeSnapshot, &l.CreatedAt); err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *Store) ListLineage(ctx context.Context) ([]state.Lineage, error) {
	rows, err := s.client.Query(ctx, `
		SELECT id, parent_id, checkpoint_id, mem_build_id, rootfs_build_id,
		       volume_snapshot, created_at
		FROM machine_lineage ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []state.Lineage
	for rows.Next() {
		var l state.Lineage
		if err := rows.Scan(&l.ID, &l.ParentID, &l.CheckpointID, &l.MemBuildID,
			&l.RootfsBuildID, &l.VolumeSnapshot, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) DeleteLineage(ctx context.Context, machineID string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM machine_lineage WHERE id = ?`, machineID); err != nil {
		return fmt.Errorf("state: delete lineage %q: %w", machineID, err)
	}
	return nil
}

// PutHandoff offers a machine to another host.
//
// Only the machine's CURRENT owner may write it, which is the whole reason the
// exception is safe: the offer is made by the host that already owns what it
// describes, so nothing is asserted across a boundary. A host writing an offer
// for somebody else's machine would be inventing permission for a third host
// to take it.
func (s *Store) PutHandoff(ctx context.Context, h *state.Handoff) error {
	if h.FromHost != s.hostID {
		return fmt.Errorf("state: host %s may not offer a machine on %s's behalf: %w",
			s.hostID, h.FromHost, state.ErrNotOwner)
	}
	if err := s.assertMachineOwner(ctx, h.MachineID, state.WriteAuth{}); err != nil {
		return err
	}
	// INSERT, never upsert: write-once is what leaves a CRDT merge nothing to
	// corrupt here.
	if _, err := s.client.Exec(ctx, `
		INSERT INTO machine_handoffs (id, machine_id, from_host, to_host, seq, created_at)
		VALUES (?,?,?,?,?,?)`,
		h.ID, h.MachineID, h.FromHost, h.ToHost, h.Seq, h.CreatedAt); err != nil {
		return fmt.Errorf("state: put handoff %q: %w", h.ID, err)
	}
	return nil
}

func (s *Store) NewestHandoff(ctx context.Context, machineID string) (*state.Handoff, error) {
	rows, err := s.client.Query(ctx, `
		SELECT id, machine_id, from_host, to_host, seq, created_at
		FROM machine_handoffs WHERE machine_id = ? ORDER BY seq DESC LIMIT 1`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var h state.Handoff
	if err := rows.Scan(&h.ID, &h.MachineID, &h.FromHost, &h.ToHost, &h.Seq, &h.CreatedAt); err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *Store) ListHandoffs(ctx context.Context) ([]state.Handoff, error) {
	rows, err := s.client.Query(ctx, `
		SELECT id, machine_id, from_host, to_host, seq, created_at
		FROM machine_handoffs ORDER BY machine_id, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []state.Handoff
	for rows.Next() {
		var h state.Handoff
		if err := rows.Scan(&h.ID, &h.MachineID, &h.FromHost, &h.ToHost, &h.Seq, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) DeleteHandoff(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM machine_handoffs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete handoff %q: %w", id, err)
	}
	return nil
}

func (s *Store) PutHostBuilds(ctx context.Context, b *state.HostBuilds) error {
	if b.HostID != s.hostID {
		return fmt.Errorf("state: host %s cannot write host %s's build row: %w",
			s.hostID, b.HostID, state.ErrNotOwner)
	}
	ids := b.Builds
	if len(ids) > state.MaxCachedBuildsPublished {
		ids = ids[:state.MaxCachedBuildsPublished]
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("state: encode host builds %q: %w", b.HostID, err)
	}
	if _, err := s.client.Exec(ctx, `
		INSERT INTO host_builds (host_id, builds, updated_at) VALUES (?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			builds=excluded.builds, updated_at=excluded.updated_at`,
		b.HostID, string(blob), b.UpdatedAt); err != nil {
		return fmt.Errorf("state: put host builds %q: %w", b.HostID, err)
	}
	return nil
}

func (s *Store) ListHostBuilds(ctx context.Context) ([]state.HostBuilds, error) {
	rows, err := s.client.Query(ctx,
		`SELECT host_id, builds, updated_at FROM host_builds ORDER BY host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.HostBuilds
	for rows.Next() {
		var b state.HostBuilds
		var blob string
		if err := rows.Scan(&b.HostID, &blob, &b.UpdatedAt); err != nil {
			return nil, err
		}
		// A row that cannot be read is an EMPTY set, never an error: all it
		// feeds is a placement bonus, so losing it costs a download while
		// failing the read would cost the create.
		if blob != "" {
			_ = json.Unmarshal([]byte(blob), &b.Builds)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) ListHostCPU(ctx context.Context) ([]state.HostCPU, error) {
	rows, err := s.client.Query(ctx,
		`SELECT host_id, vendor, cpu_template, updated_at FROM host_cpu ORDER BY host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.HostCPU
	for rows.Next() {
		var h state.HostCPU
		if err := rows.Scan(&h.HostID, &h.Vendor, &h.CPUTemplate, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PutMachineCPU records which CPU vendor photographed a memory image.
//
// The writer is whoever writes the object row this describes, so the guard is
// that row's guard: a machine's owner for a machine, the service arbiter for a
// release, and the owner of a checkpoint's machine for a checkpoint. Anything
// looser would let two hosts write one row and the merge would silently pick a
// pool the image is not in -- which is a restore against foreign CPUID, the one
// failure this whole table exists to prevent.
func (s *Store) PutMachineCPU(ctx context.Context, c *state.MachineCPU, opts ...state.WriteOption) error {
	if err := s.assertMachineCPUWriter(ctx, c, state.ResolveAuth(opts)); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
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

func (s *Store) assertMachineCPUWriter(ctx context.Context, c *state.MachineCPU, auth state.WriteAuth) error {
	switch c.Kind {
	case state.KindRelease:
		rel, err := s.GetRelease(ctx, c.ID)
		if err != nil {
			return fmt.Errorf("state: cpu row for release %q: %w", c.ID, err)
		}
		return s.assertServiceWriter(ctx, rel.ServiceID)
	case state.KindCheckpoint:
		machineID, err := s.checkpointMachine(ctx, c.ID)
		if err != nil {
			return fmt.Errorf("state: cpu row for checkpoint %q: %w", c.ID, err)
		}
		return s.assertMachineOwner(ctx, machineID, auth)
	default:
		return s.assertMachineOwner(ctx, c.ID, auth)
	}
}

// assertMachineOwner is PutMachine's guard, read rather than enforced by the
// UPDATE, because machine_cpu has no host_id column to hang a WHERE on. A
// machine with no row yet is this host's to describe: it is mid-create here.
func (s *Store) assertMachineOwner(ctx context.Context, machineID string, auth state.WriteAuth) error {
	m, err := s.GetMachine(ctx, machineID)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.HostID == s.hostID {
		return nil
	}
	// A rescue writes the cpu row while the machines row still names the dead
	// owner, so the same claim that authorises taking the machine authorises
	// this -- with liveness re-read, as ClaimMachine re-reads it.
	if auth.DeadOwnerClaim != "" && auth.DeadOwnerClaim == m.HostID {
		alive, err := s.hostIsLive(ctx, auth.DeadOwnerClaim)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		return fmt.Errorf("state: refusing the cpu row for %q taken from %s, which is "+
			"still heartbeating: %w", machineID, auth.DeadOwnerClaim, state.ErrNotOwner)
	}
	return fmt.Errorf("state: machine %q is owned by %s, not this host: %w",
		machineID, m.HostID, state.ErrNotOwner)
}

// checkpointMachine reads only the column the guard needs; there is no
// GetCheckpoint on this store and a full row would be a wider read for nothing.
func (s *Store) checkpointMachine(ctx context.Context, id string) (string, error) {
	rows, err := s.client.Query(ctx, `SELECT machine_id FROM checkpoints WHERE id = ?`, id)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", state.ErrNotFound
	}
	var machineID string
	if err := rows.Scan(&machineID); err != nil {
		return "", err
	}
	return machineID, rows.Err()
}

// DeleteMachineCPU removes the pool record for an object that is going away.
//
// A real DELETE rather than DeleteMachine's tombstone, and for DeleteService's
// reason rather than DeleteMachine's: nothing routes to or reads a cpu row by
// id once the thing it describes is gone, so the point is that it stops being
// replicated at all. The resurrection race a tombstone exists to prevent
// cannot happen here either -- the guard makes the owner of the described
// object the only writer of this row, and that is the same host doing the
// delete.
//
// The kind is read rather than passed, so a caller cannot delete a checkpoint's
// row under a machine's guard, and so removing a row needs nothing more than
// the id the caller already has.
func (s *Store) DeleteMachineCPU(ctx context.Context, id string) error {
	c, err := s.GetMachineCPU(ctx, id)
	if errors.Is(err, state.ErrNotFound) {
		// Nothing recorded, which is what every object created before this
		// table looks like. The caller asked for the row to be gone; it is.
		return nil
	}
	if err != nil {
		return err
	}
	// The same guard the write runs, which is why this must precede the delete
	// of the row it resolves through.
	if err := s.assertMachineCPUWriter(ctx, c, state.WriteAuth{}); err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx, `DELETE FROM machine_cpu WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete machine cpu %q: %w", id, err)
	}
	return nil
}

func (s *Store) GetMachineCPU(ctx context.Context, id string) (*state.MachineCPU, error) {
	rows, err := s.client.Query(ctx,
		`SELECT id, kind, vendor, last_start, last_start_at, updated_at
		 FROM machine_cpu WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var c state.MachineCPU
	if err := rows.Scan(&c.ID, &c.Kind, &c.Vendor, &c.LastStart, &c.LastStartAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, rows.Err()
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

// PutReleaseSnapshot records which checkpoint's vmstate a release restores
// from.
//
// Guarded like the release row itself, and for the same reason: it is written
// by the host running that service's deploy, the one the service arbiter
// already selected, so the writer is the same single writer and the merge has
// nothing to resolve.
func (s *Store) PutReleaseSnapshot(ctx context.Context, r *state.ReleaseSnapshot) error {
	rel, err := s.GetRelease(ctx, r.ID)
	if err != nil {
		return err
	}
	if err := s.assertServiceWriter(ctx, rel.ServiceID); err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx, `
		INSERT INTO release_snapshots (release_id, machine_id, checkpoint_id, created_at)
		VALUES (?,?,?,?)`,
		r.ID, r.MachineID, r.CheckpointID, r.CreatedAt); err != nil {
		return fmt.Errorf("state: put release snapshot %q: %w", r.ID, err)
	}
	return nil
}

func (s *Store) GetReleaseSnapshot(ctx context.Context, releaseID string) (*state.ReleaseSnapshot, error) {
	rows, err := s.client.Query(ctx, `
		SELECT release_id, machine_id, checkpoint_id, created_at
		FROM release_snapshots WHERE release_id = ?`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var r state.ReleaseSnapshot
	if err := rows.Scan(&r.ID, &r.MachineID, &r.CheckpointID, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) DeleteReleaseSnapshot(ctx context.Context, releaseID string) error {
	rel, err := s.GetRelease(ctx, releaseID)
	if err != nil {
		return err
	}
	if err := s.assertServiceWriter(ctx, rel.ServiceID); err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx,
		`DELETE FROM release_snapshots WHERE release_id = ?`, releaseID); err != nil {
		return fmt.Errorf("state: delete release snapshot %q: %w", releaseID, err)
	}
	return nil
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

const serviceVolumeCols = `id, service_id, ordinal, volume_id, created_at`

// ServiceVolume reads the volume a service mounts.
func (s *Store) ServiceVolume(ctx context.Context, serviceID string) (*state.ServiceVolume, error) {
	rows, err := s.client.Query(ctx,
		`SELECT `+serviceVolumeCols+` FROM service_volumes WHERE service_id = ? ORDER BY ordinal`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("state: service volume %q: %w", serviceID, err)
	}
	defer rows.Close()
	var out []state.ServiceVolume
	for rows.Next() {
		var id string
		var sv state.ServiceVolume
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
		return nil, fmt.Errorf("state: service volume %q: %w", serviceID, state.ErrNotFound)
	case 1:
		return &out[0], nil
	default:
		return nil, fmt.Errorf("state: service %q mounts %d volumes; this build supports one", serviceID, len(out))
	}
}

// PutServiceVolume writes the volume a service mounts.
//
// Guarded by the service's arbiter for the reason PutDomain is: the row names
// a service, so there is no host column to enforce single-writer on. Written
// once besides, so there is nothing for a merge to corrupt.
func (s *Store) PutServiceVolume(ctx context.Context, sv *state.ServiceVolume) error {
	if err := s.assertServiceWriter(ctx, sv.ServiceID); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
		INSERT INTO service_volumes (`+serviceVolumeCols+`) VALUES (?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		state.ServiceVolumeID(sv.ServiceID, sv.Ordinal), sv.ServiceID, sv.Ordinal, sv.VolumeID, sv.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: put service volume %q: %w", sv.ServiceID, err)
	}
	return nil
}

// DeleteServiceVolumes drops a service's bindings, beside DeleteService.
// DeleteServiceVolume drops one ordinal's binding.
//
// The arbiter's write, like the put: the row names a service, so there is no
// host column to enforce single-writer on and the service's arbiter is the one
// party that may change it.
func (s *Store) DeleteServiceVolume(ctx context.Context, serviceID string, ordinal int) error {
	if err := s.assertServiceWriter(ctx, serviceID); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx,
		`DELETE FROM service_volumes WHERE service_id = ? AND ordinal = ?`, serviceID, ordinal)
	if err != nil {
		return fmt.Errorf("state: unbind %s ordinal %d: %w", serviceID, ordinal, err)
	}
	return nil
}

func (s *Store) DeleteServiceVolumes(ctx context.Context, serviceID string) error {
	if _, err := s.client.Exec(ctx,
		`DELETE FROM service_volumes WHERE service_id = ?`, serviceID); err != nil {
		return fmt.Errorf("state: delete service volumes %q: %w", serviceID, err)
	}
	return nil
}

// ListServiceVolumes returns every binding, read from the local replica.
func (s *Store) ListServiceVolumes(ctx context.Context) ([]state.ServiceVolume, error) {
	rows, err := s.client.Query(ctx, `SELECT `+serviceVolumeCols+` FROM service_volumes`)
	if err != nil {
		return nil, fmt.Errorf("state: list service volumes: %w", err)
	}
	defer rows.Close()
	var out []state.ServiceVolume
	for rows.Next() {
		var id string
		var sv state.ServiceVolume
		if err := rows.Scan(&id, &sv.ServiceID, &sv.Ordinal, &sv.VolumeID, &sv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
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

// PutLabels: the writer is the host that writes the object row, checked
// the way PutMachineCPU checks it. Written once at create in practice; the
// upsert is for a retry of the same create, not for a later change.
func (s *Store) PutLabels(ctx context.Context, l *state.Labels, opts ...state.WriteOption) error {
	auth := state.ResolveAuth(opts)
	if l.Kind == "service" {
		if err := s.assertServiceWriter(ctx, l.ID); err != nil {
			return err
		}
	} else if err := s.assertMachineOwner(ctx, l.ID, auth); err != nil {
		return err
	}
	raw, err := json.Marshal(l.Labels)
	if err != nil {
		return fmt.Errorf("state: labels for %q: %w", l.ID, err)
	}
	_, err = s.client.Exec(ctx, `
		INSERT INTO machine_labels (id, kind, labels, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, labels=excluded.labels, updated_at=excluded.updated_at`,
		l.ID, l.Kind, string(raw), l.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put labels %q: %w", l.ID, err)
	}
	return nil
}

func (s *Store) GetLabels(ctx context.Context, id string) (*state.Labels, error) {
	rows, err := s.client.Query(ctx, `SELECT id, kind, labels, updated_at FROM machine_labels WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var l state.Labels
	var raw string
	if err := rows.Scan(&l.ID, &l.Kind, &raw, &l.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &l.Labels); err != nil {
		return nil, fmt.Errorf("state: labels %q are not a JSON object: %w", id, err)
	}
	return &l, nil
}

func (s *Store) DeleteLabels(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM machine_labels WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete labels %q: %w", id, err)
	}
	return nil
}

func (s *Store) PutURLAuth(ctx context.Context, u *state.URLAuth, opts ...state.WriteOption) error {
	auth := state.ResolveAuth(opts)
	if u.Kind == "service" {
		if err := s.assertServiceWriter(ctx, u.ID); err != nil {
			return err
		}
	} else if err := s.assertMachineOwner(ctx, u.ID, auth); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
		INSERT INTO url_auth (id, kind, mode, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, mode=excluded.mode, updated_at=excluded.updated_at`,
		u.ID, u.Kind, u.Mode, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put url auth %q: %w", u.ID, err)
	}
	return nil
}

func (s *Store) GetURLAuth(ctx context.Context, id string) (*state.URLAuth, error) {
	rows, err := s.client.Query(ctx, `SELECT id, kind, mode, updated_at FROM url_auth WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var u state.URLAuth
	if err := rows.Scan(&u.ID, &u.Kind, &u.Mode, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) DeleteURLAuth(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM url_auth WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete url auth %q: %w", id, err)
	}
	return nil
}

// PutBrokerGrant records what a machine or service may ask the broker for.
//
// The writer check is the same one url_auth makes, for the same reason: this
// row describes an object, so the host allowed to write it is the host that
// writes that object's row. A grant written by any other host would race
// through a CRDT merge, and two hosts disagreeing about a permission is a
// permission nobody granted.
func (s *Store) PutBrokerGrant(ctx context.Context, g *state.BrokerGrant, opts ...state.WriteOption) error {
	auth := state.ResolveAuth(opts)
	if g.Kind == "service" {
		if err := s.assertServiceWriter(ctx, g.ID); err != nil {
			return err
		}
	} else if err := s.assertMachineOwner(ctx, g.ID, auth); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
		INSERT INTO broker_grants (id, kind, org_id, scopes, sealed, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, org_id=excluded.org_id,
			scopes=excluded.scopes, sealed=excluded.sealed, updated_at=excluded.updated_at`,
		g.ID, g.Kind, g.OrgID, strings.Join(g.Scopes, ","), g.Sealed, g.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put broker grant %q: %w", g.ID, err)
	}
	return nil
}

func (s *Store) GetBrokerGrant(ctx context.Context, id string) (*state.BrokerGrant, error) {
	rows, err := s.client.Query(ctx,
		`SELECT id, kind, org_id, scopes, sealed, updated_at FROM broker_grants WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var g state.BrokerGrant
	var scopes string
	if err := rows.Scan(&g.ID, &g.Kind, &g.OrgID, &scopes, &g.Sealed, &g.UpdatedAt); err != nil {
		return nil, err
	}
	g.Scopes = state.SplitScopes(scopes)
	return &g, nil
}

func (s *Store) DeleteBrokerGrant(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM broker_grants WHERE id = ?`, id); err != nil {
		return fmt.Errorf("state: delete broker grant %q: %w", id, err)
	}
	return nil
}

// PutServiceSize records how big a service's replicas are.
//
// The writer check is the service's, not a machine's: this row describes the
// service, so the host allowed to write it is the same arbiter that writes the
// services row. Any other host writing it would race through a CRDT merge, and
// two hosts disagreeing about a size is a fleet running replicas of two sizes
// with nothing to say which is right.
func (s *Store) PutServiceSize(ctx context.Context, sz *state.ServiceSize, _ ...state.WriteOption) error {
	if err := s.assertServiceWriter(ctx, sz.ServiceID); err != nil {
		return err
	}
	_, err := s.client.Exec(ctx, `
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

func (s *Store) GetServiceSize(ctx context.Context, serviceID string) (*state.ServiceSize, error) {
	rows, err := s.client.Query(ctx, `
		SELECT service_id, vcpus, mem_mib, image_vcpus, image_mem_mib, updated_at
		FROM service_sizes WHERE service_id = ?`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var sz state.ServiceSize
	if err := rows.Scan(&sz.ServiceID, &sz.VCPUs, &sz.MemMiB,
		&sz.ImageVCPUs, &sz.ImageMemMiB, &sz.UpdatedAt); err != nil {
		return nil, err
	}
	return &sz, nil
}

// PutHostEgress records the prefix this host hands egress addresses out of.
//
// Only the host the row names may write it. The row says where one host's
// traffic leaves from, and a second host writing it would be asserting a fact
// about a machine it does not run -- which merges silently and then hands a
// tenant an address on a host that never had it.
func (s *Store) PutHostEgress(ctx context.Context, e *state.HostEgress, _ ...state.WriteOption) error {
	if e.HostID != s.hostID {
		return fmt.Errorf("state: host %s may not write %s's egress: %w",
			s.hostID, e.HostID, state.ErrNotOwner)
	}
	_, err := s.client.Exec(ctx, `
		INSERT INTO host_egress (host_id, prefix6, interface, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(host_id) DO UPDATE SET
			prefix6=excluded.prefix6, interface=excluded.interface, updated_at=excluded.updated_at`,
		e.HostID, e.Prefix6, e.Interface, e.UpdatedAt)
	if err != nil {
		return fmt.Errorf("state: put host egress %q: %w", e.HostID, err)
	}
	return nil
}

func (s *Store) ListHostEgress(ctx context.Context) ([]state.HostEgress, error) {
	rows, err := s.client.Query(ctx, `
		SELECT host_id, prefix6, interface, updated_at FROM host_egress ORDER BY host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []state.HostEgress
	for rows.Next() {
		var e state.HostEgress
		if err := rows.Scan(&e.HostID, &e.Prefix6, &e.Interface, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetHostEgress(ctx context.Context, hostID string) (*state.HostEgress, error) {
	rows, err := s.client.Query(ctx, `
		SELECT host_id, prefix6, interface, updated_at FROM host_egress WHERE host_id = ?`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, state.ErrNotFound
	}
	var e state.HostEgress
	if err := rows.Scan(&e.HostID, &e.Prefix6, &e.Interface, &e.UpdatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) DeleteHostEgress(ctx context.Context, hostID string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM host_egress WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("state: delete host egress %q: %w", hostID, err)
	}
	return nil
}

func (s *Store) DeleteServiceSize(ctx context.Context, serviceID string) error {
	if _, err := s.client.Exec(ctx, `DELETE FROM service_sizes WHERE service_id = ?`, serviceID); err != nil {
		return fmt.Errorf("state: delete service size %q: %w", serviceID, err)
	}
	return nil
}
