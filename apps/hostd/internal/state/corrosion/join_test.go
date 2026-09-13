package corrosion

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The two tables the join gate reads that the repo's schema does not own.
// Taken from a running corrosion agent at the version
// scripts/host-bootstrap.sh installs, with
//
//	SELECT sql FROM sqlite_master WHERE name = '__corro_bookkeeping_gaps'
//
// and the same for '__corro_members'. Re-take them when CORROSION_VERSION
// moves. crsql_db_versions is pinned in store_test.go as crsqlDBVersionsDDL
// and is reused here rather than copied, because two copies of a pinned shape
// drift exactly the way two copies of a rule do.
const (
	corroGapsDDL = `CREATE TABLE __corro_bookkeeping_gaps ` +
		`(actor_id BLOB NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL, ` +
		`PRIMARY KEY (actor_id, start))`

	corroMembersDDL = `CREATE TABLE __corro_members ` +
		`(actor_id BLOB PRIMARY KEY NOT NULL, address TEXT NOT NULL, ` +
		`foca_state JSON, rtts JSON DEFAULT '[]')`
)

// joinTables creates the three tables a real agent already has, so a test can
// put the replica in a named state.
func joinTables(t *testing.T, agent *fakeAgent) {
	t.Helper()
	agent.exec(t, crsqlDBVersionsDDL)
	agent.exec(t, corroGapsDDL)
	agent.exec(t, corroMembersDDL)
}

func TestVersionVectorIsPerActor(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO crsql_db_versions VALUES (x'01', 3), (x'02', 4)`)

	v, err := store.VersionVector(context.Background())
	if err != nil {
		t.Fatalf("VersionVector: %v", err)
	}
	if len(v) != 2 || v["01"] != 3 || v["02"] != 4 {
		t.Errorf("VersionVector = %v, want two actors at 3 and 4", v)
	}
}

// A sum cannot see this, which is the whole reason the gate reads the vector:
// both replicas have applied 7 changes, and they do not hold the same rows.
func TestVersionVectorSeesASkewASumHides(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO crsql_db_versions VALUES (x'01', 7), (x'02', 0)`)
	mine, err := store.VersionVector(context.Background())
	if err != nil {
		t.Fatalf("VersionVector: %v", err)
	}
	theirs := map[string]int64{"01": 0, "02": 7}

	var sumMine, sumTheirs int64
	for _, v := range mine {
		sumMine += v
	}
	for _, v := range theirs {
		sumTheirs += v
	}
	if sumMine != sumTheirs {
		t.Fatalf("the sums differ (%d, %d), so this test is not testing what it says", sumMine, sumTheirs)
	}
	behind := false
	for actor, v := range theirs {
		if mine[actor] < v {
			behind = true
		}
	}
	if !behind {
		t.Error("a per-actor comparison called these replicas equal; it must see the skew")
	}
}

func TestGapsCountsUnappliedRanges(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)

	n, err := store.Gaps(context.Background())
	if err != nil {
		t.Fatalf("Gaps: %v", err)
	}
	if n != 0 {
		t.Errorf("Gaps on a caught-up replica = %d, want 0", n)
	}

	agent.exec(t, `INSERT INTO __corro_bookkeeping_gaps VALUES (x'02', 4, 9)`)
	if n, err = store.Gaps(context.Background()); err != nil || n != 1 {
		t.Errorf("Gaps with one hole = %d (err %v), want 1", n, err)
	}
}

func TestMembersListsActorsByHex(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO __corro_members (actor_id, address) VALUES (x'02', '10.0.0.2:8787')`)

	m, err := store.Members(context.Background())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(m) != 1 || m[0] != "02" {
		t.Errorf("Members = %v, want [02]", m)
	}
}

// A read that fails is not evidence of anything. It must reach the gate as an
// error, because the gate's only safe reading of "I could not tell" is "wait".
func TestJoinReadsErrorRatherThanGuess(t *testing.T) {
	store, _ := newTestStore(t, "host-a")
	ctx := context.Background()
	if _, err := store.Gaps(ctx); err == nil {
		t.Error("Gaps with no table returned nil error")
	}
	if _, err := store.VersionVector(ctx); err == nil {
		t.Error("VersionVector with no table returned nil error")
	}
	if _, err := store.Members(ctx); err == nil {
		t.Error("Members with no table returned nil error")
	}
}

func host(id, ip string) state.Host {
	return state.Host{ID: id, PublicIP: ip, LastSeen: time.Now().Unix()}
}

// waitReady reports whether the gate opened within d.
func waitReady(g *JoinGate, d time.Duration) bool {
	select {
	case <-g.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func TestJoinGateHoldsOnAGap(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO __corro_bookkeeping_gaps VALUES (x'02', 4, 9)`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := RunJoinGate(ctx, store, JoinGateOptions{Interval: 5 * time.Millisecond})
	if waitReady(g, 100*time.Millisecond) {
		t.Fatal("the gate opened while corrosion was still filling a hole")
	}

	agent.exec(t, `DELETE FROM __corro_bookkeeping_gaps`)
	if !waitReady(g, 2*time.Second) {
		t.Error("the gate never opened after the gap was filled")
	}
	if !g.Ready() {
		t.Error("Done closed but Ready is false")
	}
}

// The state that makes self-heal claim a live machine: SWIM knows about a
// host, and this replica has applied nothing from it, so every row that host
// owns reads as absent.
func TestJoinGateHoldsWhileAMemberIsUnseen(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO __corro_members (actor_id, address) VALUES (x'02', '10.0.0.2:8787')`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := RunJoinGate(ctx, store, JoinGateOptions{Interval: 5 * time.Millisecond})
	if waitReady(g, 100*time.Millisecond) {
		t.Fatal("the gate opened with a member this replica had applied nothing from")
	}

	agent.exec(t, `INSERT INTO crsql_db_versions VALUES (x'02', 1)`)
	if !waitReady(g, 2*time.Second) {
		t.Error("the gate never opened after the member's changes arrived")
	}
}

func TestJoinGateHoldsWhileAPeerIsAhead(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO crsql_db_versions VALUES (x'01', 5), (x'02', 2)`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peer := map[string]int64{"01": 5, "02": 9}
	var mu sync.Mutex
	g := RunJoinGate(ctx, store, JoinGateOptions{
		Interval: 5 * time.Millisecond,
		Peers:    func() []state.Host { return []state.Host{host("host-b", "10.0.0.2")} },
		PeerVector: func(context.Context, state.Host) (map[string]int64, error) {
			mu.Lock()
			defer mu.Unlock()
			out := map[string]int64{}
			for k, v := range peer {
				out[k] = v
			}
			return out, nil
		},
	})
	if waitReady(g, 100*time.Millisecond) {
		t.Fatal("the gate opened while a peer was ahead on one actor")
	}

	agent.exec(t, `UPDATE crsql_db_versions SET db_version = 9 WHERE site_id = x'02'`)
	if !waitReady(g, 2*time.Second) {
		t.Error("the gate never opened after this replica caught up with the peer")
	}
}

// "I could not ask" is not evidence of being caught up.
func TestJoinGateHoldsWhileAPeerDoesNotAnswer(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var answer atomic.Bool
	g := RunJoinGate(ctx, store, JoinGateOptions{
		Interval: 5 * time.Millisecond,
		Peers:    func() []state.Host { return []state.Host{host("host-b", "10.0.0.2")} },
		PeerVector: func(context.Context, state.Host) (map[string]int64, error) {
			if !answer.Load() {
				return nil, errors.New("connection refused")
			}
			return map[string]int64{}, nil
		},
	})
	if waitReady(g, 100*time.Millisecond) {
		t.Fatal("the gate opened while a live peer was unreachable")
	}
	answer.Store(true)
	if !waitReady(g, 2*time.Second) {
		t.Error("the gate never opened after the peer answered")
	}
}

// The local rig and the first host of a new fleet: nothing to wait for.
func TestJoinGateOpensAtOnceOnASingleHostFleet(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := RunJoinGate(ctx, store, JoinGateOptions{
		Interval: 5 * time.Millisecond,
		Peers:    func() []state.Host { return nil },
		PeerVector: func(context.Context, state.Host) (map[string]int64, error) {
			t.Error("a single-host fleet asked a peer for its vector")
			return nil, nil
		},
	})
	if !waitReady(g, 2*time.Second) {
		t.Error("the gate held on a fleet with no peers and no members")
	}
}

// Shutting down must not open the gate: a host's last seconds are the worst
// time to start claiming machines on a view it never finished.
func TestJoinGateStaysClosedWhenTheContextEnds(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	agent.exec(t, `INSERT INTO __corro_bookkeeping_gaps VALUES (x'02', 4, 9)`)
	ctx, cancel := context.WithCancel(context.Background())

	g := RunJoinGate(ctx, store, JoinGateOptions{Interval: 5 * time.Millisecond})
	cancel()
	if waitReady(g, 200*time.Millisecond) {
		t.Error("the gate opened on shutdown")
	}
}

func TestOpenJoinGateIsAlreadyOpen(t *testing.T) {
	g := OpenJoinGate()
	if !g.Ready() || !waitReady(g, time.Second) {
		t.Error("OpenJoinGate returned a closed gate")
	}
}
