package machines

import (
	"context"
	"log/slog"

	"github.com/vivek7405/pilots/hostd/internal/fc"
)

// What a machine's checkpoints cost, and who is charged for it.
//
// A checkpoint is bytes in a bucket, held for as long as it exists, whether or
// not the machine is running. Nothing metered them: an agent checkpointing
// after every message grew object storage without bound, at our expense,
// invisible to the customer AND to us. Fly priced this surface twice before
// settling on it, stopped rootfs from April 2024 and volume snapshots from
// January 2026, so it is not an exotic dimension.
//
// The number is read from the durable markers rather than kept as a running
// total. A total would drift: an upload that failed halfway, a checkpoint
// deleted by retention, a host that restarted between the two, each leave a
// counter wrong in a way nothing corrects. The markers are what actually
// exists, so summing them is idempotent and self-healing.

// snapshotMiB sums what a machine's checkpoints hold, in MiB.
//
// MiB rather than GiB because a diff checkpoint is routinely tens of
// megabytes, and a GiB figure would meter most of them as nothing.
func (m *Manager) snapshotMiB(ctx context.Context, machineID string) int {
	cks, err := m.opts.Store.ListCheckpoints(ctx, machineID)
	if err != nil {
		return 0
	}
	var bytes int64
	for _, ck := range cks {
		st := fc.StatusOf(m.checkpointDir(machineID, ck.ID))
		bytes += st.Bytes
	}
	return int(bytes >> 20)
}

// remeterSnapshots brings the ledger's snapshot figure back in line with what
// this machine's checkpoints actually hold.
//
// Called after anything that changes that set: a checkpoint becoming durable,
// a checkpoint expiring, a machine's checkpoints being deleted with it. Cheap
// and idempotent, so calling it more often than strictly necessary is the safe
// mistake.
func (m *Manager) remeterSnapshots(ctx context.Context, machineID string) {
	if m.opts.Usage == nil {
		return
	}
	mib := m.snapshotMiB(ctx, machineID)
	m.opts.Usage.SetSnapshotMiB(machineID, mib)
	logSnapshotUsage(machineID, mib)
}

// SnapshotMiBOf is remeterSnapshots' read half, for the caller that rebuilds
// the ledger's open intervals after a restart.
func (m *Manager) SnapshotMiBOf(ctx context.Context, machineID string) int {
	return m.snapshotMiB(ctx, machineID)
}

// logSnapshotUsage says what a machine's checkpoints hold, once, when it
// changes by enough to be worth a line. Above a gibibyte is where a customer
// starts paying real money for something they did not ask for.
func logSnapshotUsage(machineID string, mib int) {
	if mib >= 1024 {
		slog.Info("this machine's checkpoints hold over a gibibyte",
			"machine", machineID, "mib", mib)
	}
}
