package fc

import (
	"os"
	"syscall"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// recordSnapshotSize publishes what a checkpoint actually wrote.
//
// ALLOCATED blocks, not the apparent size. A diff snapshot leaves the file at
// exactly the machine's memory size -- Firecracker merges into it in place --
// so os.Stat's Size never moves and only the block count tells you whether
// this checkpoint wrote 8MiB or 512MiB. Measuring the apparent size here
// would report every checkpoint as a full one and hide the whole lever.
func recordSnapshotSize(memPath string, memMiB int) {
	info, err := os.Stat(memPath)
	if err != nil {
		return
	}
	// syscall.Stat_t, NOT unix.Stat_t: os.FileInfo.Sys() returns the former,
	// and the two are structurally identical but distinct types -- so the
	// assertion for unix.Stat_t fails at runtime, silently, and this metric
	// publishes nothing at all. Caught only because the test asserts an
	// observation was recorded rather than that the call returned.
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	// Blocks is in 512-byte units by POSIX, regardless of the filesystem's
	// own block size.
	allocated := st.Blocks * 512
	metrics.SnapshotDiffBytes.Observe(float64(allocated))

	if memMiB > 0 {
		full := float64(int64(memMiB) << 20)
		metrics.SnapshotDiffRatio.Observe(float64(allocated) / full)
	}
}
