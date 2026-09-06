package machines

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// exitRestartWindow is how close two unasked-for exits have to be before the
// second is a crash loop rather than a transient. One automatic restart per
// window; after that the row says error and a request or an operator brings it
// back.
const exitRestartWindow = 60 * time.Second

// onExit reacts to a Firecracker that exited without being asked.
//
// It holds the machine's lock for the teardown and the row write, releases it,
// and only then restarts, because Wake takes the same lock. The restart is
// Wake and nothing else: it is idempotent, it coalesces with the router's own
// wake of the same machine, and it records the start and re-opens metering
// exactly as a wake on request does.
func (m *Manager) onExit(ctx context.Context, id string, fcm *fc.Machine, info fc.ExitInfo) {
	row, restart := m.settleExit(ctx, id, fcm, info)
	if row == nil || !restart {
		return
	}
	if err := m.Wake(ctx, id); err != nil {
		metrics.MachineExits.With("error").Inc()
		slog.Error("a machine that exited on its own could not be brought back; it stays in error",
			"machine", id, "err", err)
		return
	}
	metrics.MachineExits.With("restarted").Inc()
	slog.Info("a machine that exited on its own is running again",
		"machine", id, "url", row.Domain)
}

// settleExit is the locked half: tear down, capture, write the row, decide. It
// returns the row and whether the caller should restart it.
func (m *Manager) settleExit(ctx context.Context, id string, fcm *fc.Machine,
	info fc.ExitInfo) (*state.Machine, bool) {

	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	// A Destroy, Suspend or Redeploy that took the lock first has already
	// dealt with this process: the registry no longer holds it, or holds a
	// newer one. Either way there is nothing here to react to.
	if cur, ok := m.get(id); !ok || cur != fcm {
		return nil, false
	}
	slog.Error("a machine's firecracker exited on its own",
		"machine", id, "pid", info.Pid, "code", info.Code, "signal", info.Signal)
	appendExitRecord(fcm.SerialLog, info)

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		// No row to write, but the host must still not keep the corpse.
		slog.Error("an exited machine has no row; releasing its host resources only",
			"machine", id, "err", err)
		m.releaseProcess(id, fcm, false)
		return nil, false
	}
	// Hard rule 1: a host writes only rows describing its own machines. A row
	// this host does not run, or one that already says it is not running, is
	// somebody else's to write -- but the wreckage on THIS host is still ours
	// to clear, which is what reconcile's while-down path arrives here with.
	if row.HostID != m.opts.HostID || row.State != StateRunning {
		m.releaseProcess(id, fcm, false)
		return nil, false
	}

	// The disk first, while the block server still holds its dirty bitmap.
	// Cleanup stops that server, so the order is load-bearing.
	_, discard := m.captureDiskAfterExit(ctx, row, fcm)

	// A replica keeps its index while it is down, for the reason Suspend
	// gives: its peers resolve it by name, and the wake that brings it back
	// takes the same index through takeSlot.
	keepSlot := row.ServiceID != "" && row.ReleaseID != ""
	m.releaseProcess(id, fcm, keepSlot)

	row.State = StateError
	if !keepSlot {
		stampSlot(row, nil)
	}
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		slog.Error("could not record an exited machine's state", "machine", id, "err", err)
		return nil, false
	}
	// The process is gone, so compute stops accruing; the row and its disk are
	// still here, so wall time and storage do not -- the rule a failed wake
	// follows.
	m.opts.Usage.Transition(id, StateError)
	// Only AFTER the row names the new builds, for the reason Suspend gives.
	discard()

	// Who brings it back. A replica the rollout can replace is left to the
	// autoscaler: a fresh restore from the release is sub-second and comes
	// from a gated image, a cold boot of the crashed replica is a kernel boot.
	// A sandbox has no release to come from, and a replica holding a volume
	// has no second machine that could mount it, so both come back in place --
	// from the disk just captured when there is one, from the last suspend
	// pair when there is not.
	if row.ServiceID != "" && row.ReleaseID != "" && row.VolumeID == "" {
		metrics.MachineExits.With("replaced").Inc()
		slog.Info("an exited replica is left to the autoscaler to replace",
			"machine", id, "service", row.ServiceID)
		return row, false
	}
	if last, ok := m.exits.Load(id); ok && info.At.Sub(last.(time.Time)) < exitRestartWindow {
		metrics.MachineExits.With("error").Inc()
		slog.Error("a machine exited twice inside a minute; not restarting it again",
			"machine", id, "previous", last.(time.Time).Format(time.RFC3339))
		return row, false
	}
	m.exits.Store(id, info.At)
	return row, true
}

// releaseProcess is the host-side half of an exit: handlers, namespace,
// chroot, breadcrumbs, registry entry, slot and responder.
func (m *Manager) releaseProcess(id string, fcm *fc.Machine, keepSlot bool) {
	slotIdx := 0
	if fcm.Slot != nil {
		slotIdx = fcm.Slot.Idx
	}
	m.releaseDiscovery(id)
	if err := fcm.Cleanup(); err != nil {
		slog.Warn("an exited machine's host resources were not all released",
			"machine", id, "err", err)
	}
	m.drop(id)
	if slotIdx > 0 && !keepSlot {
		m.pool.Return(slotIdx)
	}
}

// captureDiskAfterExit stores what the block server still holds of the
// machine's disk and points the row at it. The memory image, if the row has
// one, is dropped: it describes a disk this one has moved past, the same
// disagreement recordStart clears on a cold boot.
//
// Best effort. When the block server died with the guest (the observed case:
// its control socket was gone) the writes since the last suspend are lost, the
// row keeps its last suspend pair, and the wake restores that instead. The
// returned closure removes the superseded builds and must run AFTER the row
// write.
func (m *Manager) captureDiskAfterExit(ctx context.Context, row *state.Machine,
	fcm *fc.Machine) (captured bool, discard func()) {

	discard = func() {}
	t, err := m.templateFor(ctx, row)
	if err != nil {
		slog.Warn("an exited machine's disk was not captured; it comes back from its last durable image",
			"machine", row.ID, "err", err)
		return false, discard
	}
	rootfs, err := fcm.ChunkifyDisk(ctx, fc.SnapshotOpts{
		RootfsTemplateDir: m.rootfsTemplateDir(t), BuildDir: m.buildDir(),
	})
	if err != nil {
		slog.Warn("an exited machine's disk was not captured; it comes back from its last durable image",
			"machine", row.ID, "err", err)
		return false, discard
	}
	if rootfs == uuid.Nil {
		// The block server answered, and its bitmap is empty: this machine
		// wrote nothing since it came up, so there is no newer disk than the
		// one the row already names. Touching the row here would trade a
		// working durable image for nothing -- clearing RootfsBuildID and
		// dropping the memory image leaves a row with no image at all, and
		// every later Wake fails on "no usable memory build" forever.
		return false, discard
	}
	if err := m.uploadBuild(ctx, rootfs); err != nil {
		slog.Warn("an exited machine's captured disk was not uploaded; it comes back from its last durable image",
			"machine", row.ID, "err", err)
		return false, discard
	}
	superseded := []string{row.RootfsBuildID}
	row.RootfsBuildID = rootfs.String()
	dropMem := m.discardMemoryImage(ctx, row)
	return true, func() {
		m.discardBuilds(ctx, superseded...)
		dropMem()
	}
}

// appendExitRecord writes the exit beside Firecracker's own last words, so
// `pilot machines logs` explains why the guest died.
func appendExitRecord(serialLog string, info fc.ExitInfo) {
	if serialLog == "" {
		return
	}
	f, err := os.OpenFile(serialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n%s\n", info)
}

// ExitedWhileDown is the start-time path: reconcile found breadcrumbs naming a
// process that is gone. Same reaction as an exit seen live, with no process to
// wait for.
func (m *Manager) ExitedWhileDown(ctx context.Context, st fc.State) {
	fcm := fc.AdoptedDead(st, m.opts.StateRoot, m.opts.NBDDevices)
	if fcm == nil {
		return
	}
	if st.SlotIdx > 0 {
		if slot, err := m.pool.Reserve(st.SlotIdx, st.MachineID); err == nil {
			fcm.Slot = slot
		}
	}
	// Straight into the registry rather than through put: there is no process
	// left to watch, and the exit is delivered by hand below.
	m.mu.Lock()
	m.running[st.MachineID] = fcm
	m.mu.Unlock()
	m.onExit(ctx, st.MachineID, fcm, fc.ExitInfo{
		Pid: st.Pid, Code: -1, Adopted: true, At: time.Now(),
	})
}
