package machines

import (
	"strings"
	"testing"
)

// The order is the whole content of the reclaim chain, and a reordering is
// invisible in any other test: every step still runs, the snapshot still
// happens, and the only symptom is a bigger image.
func TestReclaimChainRunsInTheRightOrder(t *testing.T) {
	trim := strings.Index(reclaimChain, "fstrim")
	sync := strings.Index(reclaimChain, "sync")
	drop := strings.Index(reclaimChain, "drop_caches")
	compact := strings.Index(reclaimChain, "compact_memory")

	for name, i := range map[string]int{
		"fstrim": trim, "sync": sync, "drop_caches": drop, "compact_memory": compact,
	} {
		if i < 0 {
			t.Fatalf("%s is missing from the reclaim chain", name)
		}
	}
	if !(trim < sync && sync < drop && drop < compact) {
		t.Errorf("chain order is fstrim=%d sync=%d drop=%d compact=%d; want "+
			"fstrim before sync before drop_caches before compact_memory",
			trim, sync, drop, compact)
	}
}

// sync must NOT be tolerated: it is the step that makes the snapshot
// internally consistent, and a guest that cannot sync is one whose disk image
// will disagree with its memory image.
func TestReclaimChainToleratesEverythingButSync(t *testing.T) {
	for _, step := range []string{"fstrim", "drop_caches", "compact_memory"} {
		i := strings.Index(reclaimChain, step)
		rest := reclaimChain[i:]
		end := strings.Index(rest, ";")
		if end < 0 {
			end = len(rest)
		}
		if !strings.Contains(rest[:end], "|| true") {
			t.Errorf("%s is not tolerated; a guest missing it would fail the "+
				"whole chain and lose the sync", step)
		}
	}
	sync := strings.Index(reclaimChain, "sync")
	syncStmt := reclaimChain[sync:]
	if end := strings.Index(syncStmt, ";"); end >= 0 {
		syncStmt = syncStmt[:end]
	}
	if strings.Contains(syncStmt, "|| true") {
		t.Error("sync is tolerated, but a failed sync means the disk and memory " +
			"images disagree and that must be visible")
	}
}
