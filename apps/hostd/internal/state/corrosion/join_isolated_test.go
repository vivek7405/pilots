package corrosion

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// A host that was told to join a fleet does not open the gate on an empty
// replica.
//
// Every input the gate reads comes from this host's OWN replica: Peers from
// its subscription cache, Members and the gap count from its tables. On a host
// that has just started and received nothing, all three said "caught up" -- so
// the gate opened on the first tick, and the emptiest possible replica opened
// it fastest. That is the exact absence the gate exists to stop a host acting
// on: self-heal was then free to claim the machines of every host it could not
// see, which was all of them.
//
// TestJoinGateOpensAtOnceOnASingleHostFleet asserts the opposite for a fleet
// of one, which is why the two cases have to be told apart by something
// OUTSIDE the replica. Dropping the Joining check makes this gate open.
func TestAJoiningHostHoldsUntilItSeesTheFleet(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := RunJoinGate(ctx, store, JoinGateOptions{
		Interval: 5 * time.Millisecond,
		Joining:  true,
		// What a fresh host's cache answers: nothing has arrived yet.
		Peers: func() []state.Host { return nil },
		PeerVector: func(context.Context, state.Host) (map[string]int64, error) {
			t.Error("a host with no peers asked one for its vector")
			return nil, nil
		},
	})
	if waitReady(g, 300*time.Millisecond) {
		t.Error("a host that was given a bootstrap peer declared itself caught up " +
			"while its replica held nothing; self-heal may now claim every " +
			"machine in the fleet")
	}
}

// And it opens as soon as a peer appears and agrees.
//
// The hold has to be temporary: a host that can never open the gate is a host
// that never serves, which would be a worse failure than the one above.
func TestAJoiningHostOpensOnceAPeerAppears(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen atomic.Bool
	g := RunJoinGate(ctx, store, JoinGateOptions{
		Interval: 5 * time.Millisecond,
		Joining:  true,
		Peers: func() []state.Host {
			if !seen.Load() {
				return nil
			}
			return []state.Host{{ID: "host-b", PublicIP: "10.0.0.2"}}
		},
		PeerVector: func(context.Context, state.Host) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
	})
	if waitReady(g, 100*time.Millisecond) {
		t.Fatal("the gate opened before any peer was visible")
	}
	seen.Store(true)
	if !waitReady(g, 2*time.Second) {
		t.Error("the gate did not open once a peer appeared and agreed")
	}
}

// A single-host fleet is unaffected: no bootstrap peer, so nothing to wait for.
// Asserted here beside the case above because the two differ only in Joining,
// and that is the whole of the distinction.
func TestALoneHostStillOpensAtOnce(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	joinTables(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := RunJoinGate(ctx, store, JoinGateOptions{
		Interval: 5 * time.Millisecond,
		Joining:  false,
		Peers:    func() []state.Host { return nil },
		PeerVector: func(context.Context, state.Host) (map[string]int64, error) {
			t.Error("a single-host fleet asked a peer for its vector")
			return nil, nil
		},
	})
	if !waitReady(g, 2*time.Second) {
		t.Error("a fleet of one held the gate")
	}
}
