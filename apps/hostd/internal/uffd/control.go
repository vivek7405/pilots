package uffd

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// cmdPrefault installs every page of the machine's memory.
//
// It exists for one moment: just before a snapshot. Firecracker reads ALL of
// guest memory to write a snapshot, and every page not yet resident faults
// through this handler while the guest is paused -- so on a machine's first
// checkpoint that is a hundred thousand faults inside the window the user
// experiences as a freeze. Measured on a 512MiB machine, it is the difference
// between a 5.8-second first checkpoint and a 450-millisecond one.
//
// Doing it eagerly at restore instead would defeat the point of lazy memory:
// a machine that touches a tenth of its pages would still hold all of them.
// This way the cost is paid only by machines that are about to be snapshotted,
// and only outside the pause.
const cmdPrefault = "prefault"

// prefaulter installs the whole memory image on demand.
type prefaulter struct {
	mu   sync.Mutex
	h    *handshake
	src  block.Slicer
	done bool
}

// control answers hostd's requests inside the handler process.
func control(p *prefaulter, stats *Stats) func(string) ([]byte, error) {
	return func(cmd string) ([]byte, error) {
		switch cmd {
		case cmdPrefault:
			return nil, p.installAll(stats)
		default:
			return nil, fmt.Errorf("unknown command %q", cmd)
		}
	}
}

// installAll walks the whole image, page by page, and installs what is missing.
//
// Serialised and idempotent: a page another worker already installed comes
// back EEXIST, which is success. Repeat calls after the first are therefore
// cheap, which matters because every checkpoint asks.
func (p *prefaulter) installAll(stats *Stats) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.h == nil || p.src == nil {
		return fmt.Errorf("uffd: not serving yet")
	}
	if p.done {
		// Nothing evicts a page installed through userfaultfd, so once the
		// whole image is in, it stays in. Walking it again costs a hundred
		// thousand ioctls that all answer EEXIST -- measured at 400ms, on the
		// path in front of every checkpoint.
		return nil
	}

	start := time.Now()
	ctx := context.Background()

	// One bulk fetch first, so the per-page reads below hit local storage
	// rather than making a round trip each.
	if err := p.src.Prefault(ctx); err != nil {
		return fmt.Errorf("uffd: bulk fetch before prefault: %w", err)
	}

	var entries []entry
	for _, r := range p.h.regions {
		for off := r.Offset; off < r.Offset+r.Size; off += p.h.pageSize {
			entries = append(entries, entry{off: int64(off), length: int64(p.h.pageSize)})
		}
	}

	before := stats.CopyEEXIST.Load()
	replay(ctx, p.h, p.src, entries, stats)

	slog.Info("uffd memory fully resident",
		"pages", len(entries),
		"already_present", stats.CopyEEXIST.Load()-before,
		"ms", time.Since(start).Milliseconds())
	p.done = true
	return nil
}
