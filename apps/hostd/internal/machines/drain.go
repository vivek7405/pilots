package machines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Emptying a host on purpose.
//
// # Why this exists
//
// A machine moved only when its host was provably dead. That makes every
// planned thing a host needs -- a kernel upgrade, a reboot, a disk swap,
// retiring the box -- customer-visible: the operator either takes the machines
// down with the host, or does not do the maintenance.
//
// Rule 4 is what makes the fix nearly free. A machine's URL is permanent and
// its state lives in object storage, so moving one is: suspend it, offer it,
// let the target restore it. The id, the name, the URL and the disk are
// untouched; what changes is which host answers for it.
//
// # Why it is ordered
//
// Suspended and stopped machines go first. Moving one is a row write and
// nothing else -- no customer sees anything at all -- so doing them first
// empties most of a host before anything visible happens.
//
// Running machines without a volume go next. Each pays a suspend and a
// restore, which is under a second, and a request arriving in between is held
// by the router rather than refused.
//
// Volume-backed machines go last, and they are the expensive ones: a volume
// has exactly one writer, so the replacement cannot mount it until this host
// has let go, and the window is an unmount plus a mount plus a boot. Still
// held, never failed, but seconds rather than milliseconds.
//
// # What is NOT done here
//
// Nothing is copied. The source uploads no image to the target, because there
// is nothing to upload: the suspend already put the machine in object storage,
// which is the only place its state was ever truly kept.

// drainConcurrency is how many machines move at once.
//
// Small. Each move is a suspend on this host and a restore on another, and
// running twenty at once would make the drain a load spike on whichever host
// is receiving them -- which is the host that just told the fleet it had room.
const drainConcurrency = 4

// handoffTimeout is how long the source waits for a target to take a machine
// before offering it to somebody else.
const handoffTimeout = 60 * time.Second

// maxHandoffTargets bounds how many hosts one machine is offered to. After
// that it is left suspended here and reported: a machine that no host will
// take is a fleet-capacity problem, and spinning on it hides that.
const maxHandoffTargets = 3

// HandoffNotifier tells another host to take a machine.
//
// An interface so this package does not learn how to call a peer, and so a
// single box simply has none. Best effort by design: the offer row is what
// authorises the move, and a host that never gets the call still takes the
// machine when it reads the row.
// The method is Offer rather than Take deliberately. `Take` on a selector is
// how this package's slot pool is consumed, and a structural test enumerates
// every caller of it to keep a bring-up from taking its own index -- a name
// collision here would make that test unable to tell a slot take from a peer
// call, and the next real mistake would pass it.
type HandoffNotifier interface {
	Offer(ctx context.Context, hostID, machineID, handoffID string) error
}

// SetHandoffs installs the peer notifier after construction.
//
// Late because the manager is built before the fleet's peer client exists:
// every netns slot's address derives from the mesh key, so the manager has to
// come first. Nil until then is correct rather than merely tolerable -- a host
// with no peers has nobody to hand a machine to.
func (m *Manager) SetHandoffs(h HandoffNotifier) { m.opts.Handoffs = h }

// ErrDraining is what every lifecycle path answers for a machine that is being
// moved. Distinct so the API can say "moving to <host>" rather than a generic
// conflict: the operation will work again shortly, somewhere else.
var ErrDraining = errors.New("machine is moving to another host")

// DrainResult is what a drain did, per machine.
type DrainResult struct {
	Moved   []string
	Left    []string
	Errors  map[string]string
	Started int64
}

// Drain moves every machine off this host.
//
// pick chooses where each machine goes; it is handed the row and returns a
// host id, or false when there is nowhere to put it. Injected rather than
// ranked here because ranking needs the fleet's rows, and this package owns
// machines rather than the fleet.
func (m *Manager) Drain(ctx context.Context, pick func(state.Machine) (string, bool)) (*DrainResult, error) {
	// Stop taking new work FIRST. A drain that moved machines while the placer
	// kept sending new ones would never converge; the flag is published on the
	// next heartbeat and every ranker skips a draining host.
	m.SetDraining(true)

	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, fmt.Errorf("machines: read this host's machines to drain: %w", err)
	}
	mine := make([]state.Machine, 0, len(rows))
	for _, row := range rows {
		if row.HostID == m.opts.HostID && row.State != state.StateDestroyed {
			mine = append(mine, row)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return drainOrder(mine[i]) < drainOrder(mine[j]) })

	out := &DrainResult{Errors: map[string]string{}, Started: time.Now().Unix()}
	var mu sync.Mutex
	sem := make(chan struct{}, drainConcurrency)
	var wg sync.WaitGroup

	for _, row := range mine {
		wg.Add(1)
		go func(row state.Machine) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			err := m.handOff(ctx, row, pick)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				out.Moved = append(out.Moved, row.ID)
			default:
				out.Left = append(out.Left, row.ID)
				out.Errors[row.ID] = err.Error()
			}
		}(row)
	}
	wg.Wait()

	sort.Strings(out.Moved)
	sort.Strings(out.Left)
	slog.Info("drain finished", "moved", len(out.Moved), "left", len(out.Left))
	return out, nil
}

// drainOrder sorts the cheapest moves first. See the file comment: the aim is
// to empty most of the host before anything a customer could notice happens.
func drainOrder(row state.Machine) int {
	switch {
	case row.State != StateRunning:
		return 0 // a row write, and nothing else
	case row.VolumeID == "":
		return 1 // suspend and restore, sub-second, held by the router
	default:
		return 2 // unmount, mount, boot: seconds, still held
	}
}

// handOff moves one machine, trying a bounded number of targets.
func (m *Manager) handOff(ctx context.Context, row state.Machine,
	pick func(state.Machine) (string, bool)) error {

	tried := map[string]bool{}
	var lastErr error

	for attempt := 1; attempt <= maxHandoffTargets; attempt++ {
		target, ok := pick(row)
		if !ok || tried[target] {
			if lastErr != nil {
				return lastErr
			}
			return errors.New("no other host can take this machine")
		}
		tried[target] = true

		if err := m.offerTo(ctx, row, target, attempt); err != nil {
			lastErr = err
			slog.Warn("a machine could not be handed to a host; trying another",
				"machine", row.ID, "target", target, "attempt", attempt, "err", err)
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no host took this machine")
}

// offerTo suspends the machine, writes the offer, and waits for the target to
// take it.
//
// The ORDER is the contract. The suspended write goes first, so the row says
// the machine is down before anything says who may take it -- a target that
// acted on the offer first would find a running machine and be refused, which
// is exactly what the store's claim check is for, but the source should not be
// the one creating that race.
func (m *Manager) offerTo(ctx context.Context, row state.Machine, target string, seq int) error {
	// Refuse every other lifecycle path for the duration. Without this a wake
	// racing the handoff would bring the machine up here just as another host
	// claims it, and both would believe they hold it.
	if !m.beginHandoff(row.ID, target) {
		return errors.New("this machine is already being moved")
	}
	defer m.endHandoff(row.ID)

	fresh, err := m.opts.Store.GetMachine(ctx, row.ID)
	if err != nil {
		return err
	}
	if fresh.HostID != m.opts.HostID {
		return nil // it already left, which is the outcome we wanted
	}

	if fresh.State == StateRunning {
		// The suspend IS the checkpoint the target restores. A named
		// checkpoint would work and would add a row the tenant can see, for an
		// operation they did not ask for.
		if err := m.Suspend(ctx, row.ID); err != nil {
			return fmt.Errorf("suspend before handing over: %w", err)
		}
	}

	// A volume follows the machine, and it can only be mounted by one host, so
	// this host lets go BEFORE the offer rather than after. The local cache
	// goes with it: a stale copy of a volume's image or its metadata is the
	// page-cache landmine, and the only way to be sure of it is for no copy to
	// survive the unmount.
	//
	// releaseVolume is the destroy path's own, reused deliberately: it
	// unmounts and clears BOTH machine_id and host_id, and a volume still
	// naming this host is one no other host will ever mount. A second
	// implementation of "let go of a volume" would be a second place for that
	// to be got wrong.
	if fresh.VolumeID != "" && m.opts.Volumes != nil {
		if err := m.releaseVolume(ctx, fresh.VolumeID); err != nil {
			return fmt.Errorf("release the volume before handing over: %w", err)
		}
	}

	offer := &state.Handoff{
		ID:        newID("ho"),
		MachineID: row.ID,
		FromHost:  m.opts.HostID,
		ToHost:    target,
		Seq:       seq,
		CreatedAt: time.Now().Unix(),
	}
	if err := m.opts.Store.PutHandoff(ctx, offer); err != nil {
		return fmt.Errorf("offer the machine: %w", err)
	}
	slog.Info("offered a machine to another host",
		"machine", row.ID, "target", target, "handoff", offer.ID, "seq", seq)

	if m.opts.Handoffs != nil {
		// Tell the target rather than waiting for it to notice. Best effort:
		// the offer row is the authority, so a failed call costs the wait
		// below rather than the handoff.
		if err := m.opts.Handoffs.Offer(ctx, target, row.ID, offer.ID); err != nil {
			slog.Warn("could not tell the target about a handoff; waiting for it to notice",
				"machine", row.ID, "target", target, "err", err)
		}
	}

	return m.awaitHandoff(ctx, row.ID, target)
}

// awaitHandoff waits until the local replica agrees the machine has moved.
func (m *Manager) awaitHandoff(ctx context.Context, id, target string) error {
	deadline := time.Now().Add(handoffTimeout)
	for {
		row, err := m.opts.Store.GetMachine(ctx, id)
		if err == nil && row.HostID == target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not take the machine within %s", target, handoffTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Take accepts a machine another host has offered.
//
// The mirror of Rescue, with one difference that is the whole point: the claim
// is authorised by the OFFER rather than by the old owner being dead, and the
// old owner is very much alive.
func (m *Manager) Take(ctx context.Context, id, handoffID string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := m.get(id); ok {
		return nil // already here
	}

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return fmt.Errorf("machines: read %s before taking it: %w", id, err)
	}
	if row.HostID == m.opts.HostID {
		return nil // already ours
	}

	// Every check of the offer happens inside the store, against the rows
	// rather than against anything the caller said. See claimByHandoff.
	if err := m.opts.Store.ClaimMachine(ctx, id, m.opts.HostID, row.State,
		state.WithHandoff(handoffID)); err != nil {
		return fmt.Errorf("machines: take %s: %w", id, err)
	}

	fresh, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return fmt.Errorf("machines: re-read %s after taking it: %w", id, err)
	}
	// The slot index on the row is the SOURCE host's. Cleared for the reason
	// Rescue clears it: this host now owns the row, so the index would be read
	// as one of ours and would point peers at whatever local machine really
	// holds it.
	stampSlot(fresh, nil)
	fresh.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, fresh); err != nil {
		return fmt.Errorf("machines: clear %s's old slot after taking it: %w", id, err)
	}

	// A machine that was STOPPED stays stopped. It was not running before the
	// drain and starting it here would be a change the operator did not ask
	// for; the row moved, which is all a drain owes it.
	if fresh.State == StateStopped {
		slog.Info("took a stopped machine", "machine", id, "handoff", handoffID)
		return nil
	}

	fcm, kind, err := m.bringUp(ctx, fresh)
	if err != nil {
		fresh.State = StateError
		stampSlot(fresh, nil)
		fresh.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, fresh)
		return fmt.Errorf("machines: bring up %s after taking it: %w", id, err)
	}
	m.put(id, fcm)

	fresh.State = StateRunning
	stampSlot(fresh, fcm)
	fresh.LastActivity = time.Now().Unix()
	fresh.UpdatedAt = fresh.LastActivity
	discard := m.recordStart(ctx, fresh, kind)
	if err := m.opts.Store.PutMachine(ctx, fresh); err != nil {
		return err
	}
	org := ""
	if t, terr := m.opts.Store.GetTenancy(ctx, fresh.ID); terr == nil && t != nil {
		org = t.OrgID
	}
	// This host bills it from here. The source closed its interval when it
	// suspended, so the seam is the handoff itself and neither side
	// double-bills.
	m.opts.Usage.Open(fresh.ID, org, StateRunning, fresh.VCPUs, fresh.MemMiB,
		m.volumeGiB(ctx, fresh.VolumeID))
	discard()

	slog.Info("took a machine from another host",
		"machine", id, "from", row.HostID, "handoff", handoffID)
	return nil
}

// beginHandoff marks a machine as moving, so every other lifecycle path
// refuses it. False when one is already in progress.
func (m *Manager) beginHandoff(id, target string) bool {
	_, loaded := m.handingOff.LoadOrStore(id, target)
	return !loaded
}

func (m *Manager) endHandoff(id string) { m.handingOff.Delete(id) }

// HandingOff reports the host a machine is moving to, when one is.
//
// Read by every lifecycle path, so a wake, a schedule or the idle monitor
// refuses rather than racing the move. Read by the router too, which forwards
// the request to the target instead of failing it.
func (m *Manager) HandingOff(id string) (string, bool) {
	v, ok := m.handingOff.Load(id)
	if !ok {
		return "", false
	}
	target, _ := v.(string)
	return target, true
}
