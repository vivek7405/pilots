package machines

import (
	"context"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// reclaimChain is what the guest runs immediately before a snapshot.
//
// fstrim releases blocks the guest has freed, so the disk diff shrinks and
// the block cache stops carrying dead extents. sync is the one that makes the
// snapshot internally consistent (see reclaimGuestMemory). drop_caches turns
// page-cache pages into pages the memory image does not have to carry.
// compact_memory is LAST, because it moves what survives into fewer pages --
// and under 2MiB backing that is the difference between a compact diff and a
// scattered one.
//
// One exec rather than four: each is a round trip into the guest, and this
// runs inside the window before a pause.
//
// Every step but sync is tolerated individually. A guest whose filesystem has
// no discard support, or a kernel without one of the procfs knobs, still
// wants the sync -- and slightly less hygiene beats no snapshot.
// The chain's exit status is sync's. Every other step ends in `|| true`, so
// without the `|| exit 1` a failed sync would be papered over by the last
// tolerated step's zero and the warning below could never fire for it.
const reclaimChain = "fstrim -a 2>/dev/null || true; sync || exit 1; " +
	"echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true; " +
	"echo 1 > /proc/sys/vm/compact_memory 2>/dev/null || true"

// reclaimGuestMemory makes the guest write its dirty pages to the virtual disk
// and hand back what it is no longer using.
//
// Without the sync, a snapshot is internally inconsistent. The guest buffers
// file writes in its own page cache, so a write made moments before a snapshot
// lives in guest MEMORY but not yet on the virtual disk. The memory image
// captures it; the disk image does not. On restore the two disagree, and a
// file the guest is certain it wrote is missing -- which surfaces much later
// as inexplicable data loss rather than as a snapshot bug.
//
// The rest of the chain shrinks what the snapshot has to carry at all. A page
// the guest has released is still a dirty page from the host's point of view
// until the guest says otherwise, so it lands in the next diff and is faulted
// back in on the next wake.
//
// A failure here is logged rather than fatal: a snapshot with a slightly stale
// disk is far better than no snapshot at all, and the caller is usually
// suspending because the machine is idle anyway.
func (m *Manager) reclaimGuestMemory(ctx context.Context, machineID string) {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		return
	}

	// Short timeout: sync on an idle machine is near-instant, and a machine
	// that cannot answer is one we should snapshot anyway rather than wait for.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, err := m.execOnSlot(ctx, machineID, slot, api.ExecRequest{
		Cmd: reclaimChain, User: "root", TimeoutMS: 10_000,
	})
	if err != nil {
		slog.Warn("could not flush the guest's disk before snapshotting; "+
			"the snapshot may miss its most recent writes",
			"machine", machineID, "err", err)
		return
	}
	if out.ExitCode != 0 {
		slog.Warn("the guest's sync failed before snapshotting; "+
			"the snapshot may miss its most recent writes",
			"machine", machineID, "exit", out.ExitCode, "stderr", out.Stderr)
	}
}
