package quota

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// How much object storage an org's checkpoints hold.
//
// Read from what actually exists rather than from a counter. A counter drifts
// the moment an upload fails halfway, a retention pass deletes something, or a
// host restarts between the two, and nothing ever corrects it; summing the
// markers is idempotent and self-healing, which is the same reasoning the
// meter uses.
//
// # The one compromise
//
// The bytes live in each checkpoint's LOCAL staging directory, so this counts
// only what this host can see. On a fleet an org's checkpoints are spread over
// the hosts that took them, and a host therefore under-counts. That is the
// wrong direction for a hard cap and the right one for a soft one: the limit
// admits a caller who is over it rather than refusing one who is not, and the
// meter, which is what an invoice is built from, is summed across every host
// by the dashboard and is exact.
//
// Making it exact here would mean a replicated per-checkpoint byte count,
// which is a Corrosion table written on every checkpoint on every host: a
// large amount of gossip to sharpen a limit whose job is to catch the
// unbounded case. The retention policy is what actually bounds growth.

// CheckpointRoot is where a host stages its checkpoints. A variable so a test
// can point it somewhere writable; hostd sets it from its cache root.
var CheckpointRoot = "/var/cache/pilots/machines"

// SetCheckpointRoot tells this package where checkpoints are staged.
func SetCheckpointRoot(dir string) {
	if dir != "" {
		CheckpointRoot = dir
	}
}

// snapshotGiBOf sums the checkpoints of an org's machines, in GiB.
func snapshotGiBOf(ctx context.Context, st state.Store, owned map[string]struct{}) int {
	var bytes int64
	for id := range owned {
		cks, err := st.ListCheckpoints(ctx, id)
		if err != nil {
			continue
		}
		for _, ck := range cks {
			dir := filepath.Join(CheckpointRoot, id, "checkpoints", ck.ID)
			if _, err := os.Stat(dir); err != nil {
				// Not staged here: a checkpoint another host took. See the
				// package comment for why under-counting is the right
				// direction for a soft cap.
				continue
			}
			bytes += fc.StatusOf(dir).Bytes
		}
	}
	gib := int(bytes >> 30)
	if gib > 0 {
		slog.Debug("counted an org's checkpoint storage", "gib", gib)
	}
	return gib
}
