package machines

import (
	"context"
	"log/slog"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// flushGuestDisk makes the guest write its dirty pages to the virtual disk.
//
// Without this, a snapshot is internally inconsistent. The guest buffers file
// writes in its own page cache, so a write made moments before a snapshot
// lives in guest MEMORY but not yet on the virtual disk. The memory image
// captures it; the disk image does not. On restore the two disagree, and a
// file the guest is certain it wrote is missing -- which surfaces much later
// as inexplicable data loss rather than as a snapshot bug.
//
// A failure here is logged rather than fatal: a snapshot with a slightly stale
// disk is far better than no snapshot at all, and the caller is usually
// suspending because the machine is idle anyway.
func (m *Manager) flushGuestDisk(ctx context.Context, machineID string) {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		return
	}

	// Short timeout: sync on an idle machine is near-instant, and a machine
	// that cannot answer is one we should snapshot anyway rather than wait for.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if _, err := m.execOnSlot(ctx, machineID, slot, api.ExecRequest{
		Cmd: "sync", User: "root", TimeoutMS: 10_000,
	}); err != nil {
		slog.Warn("could not flush the guest's disk before snapshotting; "+
			"the snapshot may miss its most recent writes",
			"machine", machineID, "err", err)
	}
}
