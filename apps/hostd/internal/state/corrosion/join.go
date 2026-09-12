package corrosion

import (
	"context"
	"fmt"
)

// What a replica knows about how far along it is.
//
// Version() sums the version vector into one comparable number, which answers
// "are these two hosts far apart" and nothing else. Joining needs more than
// that: a sum can match while the two replicas hold different rows, and a sum
// says nothing about whether corrosion is still filling holes in what it has
// applied. So the join gate reads the vector per actor, the gap table, and the
// membership list, and compares them itself.
//
// All three tables are cr-sqlite's or corrosion's, not this schema's. Their
// shapes are pinned in join_test.go, taken from a running agent at the version
// scripts/host-bootstrap.sh installs. An error on any of them reads as "not
// complete" at the caller rather than as a guess, because the only safe
// reading of "I could not tell" is "do not act yet".

// VersionVector is the per-actor version this replica has applied: one entry
// per host that has ever written, keyed by the actor's site id in hex.
//
// The sum of this map is Version(). The map itself is what a comparison needs,
// because two replicas can sum alike while one is ahead on actor A and behind
// on actor B, which is exactly the state that makes a claim wrong.
func (s *Store) VersionVector(ctx context.Context) (map[string]int64, error) {
	rows, err := s.client.Query(ctx, `SELECT hex(site_id), db_version FROM crsql_db_versions`)
	if err != nil {
		return nil, fmt.Errorf("state: version vector: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var actor string
		var v int64
		if err := rows.Scan(&actor, &v); err != nil {
			return nil, fmt.Errorf("state: version vector: %w", err)
		}
		out[actor] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: version vector: %w", err)
	}
	return out, nil
}

// Gaps counts the holes corrosion is still filling.
//
// __corro_bookkeeping_gaps holds one row per range of an actor's changes this
// replica knows it has NOT applied: it was told they exist and is fetching
// them. A replica with gaps is not behind in the harmless sense of "has not
// heard yet", it is behind in the sense of "knows it is missing rows", and a
// row that is missing reads exactly like a row that was never written.
func (s *Store) Gaps(ctx context.Context) (int, error) {
	rows, err := s.client.Query(ctx, `SELECT COUNT(*) FROM __corro_bookkeeping_gaps`)
	if err != nil {
		return 0, fmt.Errorf("state: gaps: %w", err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, fmt.Errorf("state: gaps: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("state: gaps: %w", err)
	}
	return n, nil
}

// Members lists the actors SWIM has told this host about, by site id in hex.
//
// Corrosion's membership comes from foca, whose member list is the OTHER
// members: a host is not a member of its own view. So every id here is a peer,
// and a peer with no entry in the version vector is a host this replica has
// heard of but applied nothing from. That is the state a bare sum cannot see
// and the one that makes self-heal claim a live machine.
//
// If a future corrosion did include self, the only cost is that the gate waits
// for this host's own first write to land in the vector, which the heartbeat
// does within one interval. Waiting is the safe direction.
func (s *Store) Members(ctx context.Context) ([]string, error) {
	rows, err := s.client.Query(ctx, `SELECT hex(actor_id) FROM __corro_members`)
	if err != nil {
		return nil, fmt.Errorf("state: members: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			return nil, fmt.Errorf("state: members: %w", err)
		}
		out = append(out, actor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: members: %w", err)
	}
	return out, nil
}
