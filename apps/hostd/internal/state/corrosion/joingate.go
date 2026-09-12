package corrosion

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The join gate: a host that has just joined the fleet does not act on the
// ABSENCE of a row until its replica has caught up.
//
// # Why a gate and not a retry
//
// Self-heal's job is to claim the machines of hosts it cannot see. On a fully
// replicated replica "cannot see" means "is dead". On a half-replicated one it
// also means "is alive and I have not applied its rows yet", and the two are
// indistinguishable from inside the query: both are an empty result. Nothing
// errors, nothing conflicts, and the claim merges cleanly into a row a live
// host is still writing. The damage is silent and it is not recoverable by
// retrying, because both hosts now believe they own the machine.
//
// So the rule is a principle, not a list: BEFORE the gate opens a host may act
// on its own rows, and on the PRESENCE of a foreign row, but never on the
// absence of one. Serving HTTP, reconciling its own machines, heartbeating,
// suspending its own idle machines, metering, answering DNS for rows it can
// see and routing to hosts it can see are all reads of presence, and all stay
// ungated: gating them would make a joining host useless without making the
// fleet safer. What waits is every write a partial view can misdirect, which
// today is three callers: self-heal claims, the router's held-request rescue,
// and autoscaler arbitration.
//
// # What complete means
//
// Three conditions, all read from the local replica in one pass, plus one read
// per live peer:
//
//  1. No gaps. __corro_bookkeeping_gaps is empty, so corrosion is not still
//     filling holes it knows about.
//  2. No unseen member. Every actor SWIM has told us about has an entry in our
//     version vector, so there is no host we have heard of but applied nothing
//     from.
//  3. No peer ahead of us, per actor. For every live peer, that peer's view of
//     every actor is at or below ours. A peer that does not answer keeps the
//     gate closed, because "I could not ask" is not evidence of being caught
//     up.
//
// A single-host fleet has no peers and no members, so it is complete on the
// first pass. That is the local rig and the first host of a new fleet, neither
// of which should wait for anything.
//
// The shape is uncloud's (docs/prior-art/uncloud.md COPY 1): a join-only
// latch, checked once at startup, never re-closed. A gate that could re-close
// would be a liveness dependency on gossip, which is the control plane rule 1
// forbids: a host that has caught up once has its own rows and can serve them
// forever, whatever the mesh does afterwards.

// JoinGate is the latch. It starts closed and opens once.
type JoinGate struct {
	ready atomic.Bool
	done  chan struct{}
	once  sync.Once
}

// Ready reports whether the replica has caught up. Callers that can be asked
// to act at any moment (self-heal's tick, the router's rescuer lookup) read
// this; callers with a startup of their own wait on Done.
func (g *JoinGate) Ready() bool { return g.ready.Load() }

// Done closes when the gate opens. It is closed exactly once, so a receive on
// it is safe from any number of goroutines and safe after the fact.
func (g *JoinGate) Done() <-chan struct{} { return g.done }

func (g *JoinGate) open() {
	g.once.Do(func() {
		g.ready.Store(true)
		close(g.done)
	})
}

// OpenJoinGate returns a gate that is already open. It is for tests, for the
// SQLite store (which has no replication to wait for), and for any caller that
// must not be gated.
func OpenJoinGate() *JoinGate {
	g := &JoinGate{done: make(chan struct{})}
	g.open()
	return g
}

// JoinGateOptions is what the gate needs to decide. Every hook is supplied by
// the caller so this package keeps its existing dependencies: the gate reads
// the fleet through the same cache everything else does.
type JoinGateOptions struct {
	// Peers lists the live hosts OTHER than this one. Empty means a single-host
	// fleet, which is complete immediately.
	Peers func() []state.Host
	// PeerVector fetches one peer's per-actor version vector, over whatever
	// transport the caller uses (in hostd, the plain listener's /v1/health).
	// An error, or a peer that does not answer, keeps the gate closed.
	PeerVector func(ctx context.Context, host state.Host) (map[string]int64, error)
	// Interval between passes. Zero means two seconds.
	Interval time.Duration
	// Log is where the periodic "still waiting" line goes. Zero means the
	// default logger.
	Log *slog.Logger
}

// skipJoinGate reports whether the gate must open at once, reproducing the
// pre-gate bug on purpose for the hostility battery.
//
// Two flags, both required, the same shape as internal/nbd/faults.go: an
// assertion that the gate holds proves nothing unless the same run can show
// the claim happening when the gate is removed. gate.sh section 29 is that
// negative control. Neither flag has a default that arms anything.
func skipJoinGate() bool {
	return os.Getenv("PILOT_FAULTS") == "1" && os.Getenv("PILOT_FAULT_SKIP_JOIN_GATE") == "1"
}

// RunJoinGate starts the gate and returns it immediately. The gate is closed
// until the replica has caught up; the caller keeps serving in the meantime.
//
// The context ending does NOT open the gate. A host shutting down must not
// spend its last seconds claiming machines on a view it never finished.
func RunJoinGate(ctx context.Context, store *Store, opts JoinGateOptions) *JoinGate {
	g := &JoinGate{done: make(chan struct{})}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if skipJoinGate() {
		log.Warn("join gate skipped by fault injection; this host may claim live machines")
		g.open()
		return g
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		var lastLog time.Time
		for {
			gaps, unseen, behind, err := joinState(ctx, store, opts)
			if err == nil && gaps == 0 && unseen == 0 && behind == 0 {
				metrics.ReplicationGaps.Set(0)
				metrics.ReplicationComplete.Set(1)
				log.Info("replication complete; this host may now claim machines")
				g.open()
				return
			}
			metrics.ReplicationGaps.Set(int64(gaps))
			metrics.ReplicationComplete.Set(0)
			if time.Since(lastLog) >= 30*time.Second {
				lastLog = time.Now()
				log.Warn("replication not complete; claiming nothing yet",
					"gaps", gaps, "members_unseen", unseen, "peers_behind", behind,
					"err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return g
}

// joinState runs one pass. The counts are for the log line and the gauge; the
// error is why a pass could not decide, which reads as not complete.
func joinState(ctx context.Context, store *Store, opts JoinGateOptions) (gaps, unseen, behind int, err error) {
	gaps, err = store.Gaps(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	mine, err := store.VersionVector(ctx)
	if err != nil {
		return gaps, 0, 0, err
	}
	members, err := store.Members(ctx)
	if err != nil {
		return gaps, 0, 0, err
	}
	for _, actor := range members {
		if _, ok := mine[actor]; !ok {
			unseen++
		}
	}
	if opts.Peers == nil || opts.PeerVector == nil {
		return gaps, unseen, 0, nil
	}
	for _, peer := range opts.Peers() {
		theirs, perr := opts.PeerVector(ctx, peer)
		if perr != nil {
			behind++
			if err == nil {
				err = perr
			}
			continue
		}
		for actor, v := range theirs {
			if mine[actor] < v {
				behind++
				break
			}
		}
	}
	return gaps, unseen, behind, err
}
