package uffd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// A restore does not fault randomly. The guest touches roughly the same pages
// in roughly the same order every time, so the order from one wake is a good
// prediction of the next. Recording it and replaying it turns a wake from
// thousands of serialised faults into one bulk fetch and a burst of copies.
const (
	// prefetchFetchWorkers hides per-request latency to object storage.
	prefetchFetchWorkers = 16
	// prefetchCopyWorkers keeps the kernel's queue draining while pages
	// are still arriving.
	prefetchCopyWorkers = 4
	// prefetchDiffCap bounds the ranges taken from the last cycle's diff.
	//
	// Uncapped, a machine that rewrote most of its memory turns its wake into
	// a full image read -- the fault storm the bulk fetch exists to avoid,
	// moved one layer up. 64MiB is an eighth of the default 512MiB machine.
	prefetchDiffCap = 64 << 20
)

// diffEntries are the ranges a build stores ITSELF, which is exactly what the
// machine changed since the template it was diffed against.
//
// The set costs nothing to obtain: it is the self-pointing half of the
// mapping the build already carries, so this needs no new persisted state.
// The pages a machine dirtied last cycle are the best available predictor of
// what it will touch next cycle, and the recorded fault order cannot cover a
// page the previous run never faulted in order.
//
// Returned ascending by offset, capped, with the tail dropped and counted.
func diffEntries(src block.Slicer, pageSize uint64) ([]entry, int) {
	hs, ok := src.(block.HeaderedSlicer)
	if !ok {
		return nil, 0
	}
	hdr := hs.Header()
	if hdr == nil || hdr.Metadata == nil {
		return nil, 0
	}
	self := hdr.Metadata.BuildId

	// A build mapping is aligned to the BLOCK size (4KiB), and the guest
	// faults at the PAGE size, which under hugepage backing is 2MiB. Emitting
	// a mapping's own offset would hand UFFDIO_COPY a destination that is not
	// page-aligned, and the kernel rejects every one of them -- 469 failed
	// copies out of 481 on the first hugepage host this ran on, which is how
	// this was found. Align down, cover the mapping's whole span, and
	// deduplicate: at 2MiB many 4KiB mappings fall inside one guest page.
	seen := make(map[uint64]struct{})
	var out []entry
	var span uint64
	var dropped int
	for _, m := range hdr.Mapping {
		if m == nil || m.BuildId != self {
			continue // served by the parent: unchanged, and not worth fetching
		}
		for off := m.Offset &^ (pageSize - 1); off < m.Offset+m.Length; off += pageSize {
			if _, ok := seen[off]; ok {
				continue
			}
			if span+pageSize > prefetchDiffCap {
				dropped++
				continue
			}
			seen[off] = struct{}{}
			out = append(out, entry{off: int64(off), length: int64(pageSize)})
			span += pageSize
		}
	}
	return out, dropped
}

// recorder appends the fault order to a file, for the next wake to replay.
type recorder struct {
	mu   sync.Mutex
	file *os.File
}

// newRecorder creates the capture file. A nil path disables recording.
func newRecorder(path string) (*recorder, error) {
	if path == "" {
		return &recorder{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("uffd: mkdir for prefetch capture: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("uffd: create prefetch capture: %w", err)
	}
	return &recorder{file: f}, nil
}

func (r *recorder) record(off, length int64) {
	if r == nil || r.file == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.file, "%d %d\n", off, length)
}

func (r *recorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// readPrefetch loads a recorded fault order.
//
// It must be called BEFORE the recorder is created. The two commonly name the
// same path -- that is the point, each wake refines the last one's list -- and
// os.Create truncates, so creating first reads back an empty file and silently
// disables the replay it was meant to drive.
func readPrefetch(path string) []byte {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// A machine's first wake has no list. Not an error.
		slog.Info("no prefetch list to replay", "path", path, "err", err)
		return nil
	}
	return raw
}

// entry is one recorded fault.
type entry struct {
	off    int64
	length int64
}

func parsePrefetch(contents []byte) []entry {
	var entries []entry

	s := bufio.NewScanner(bytes.NewReader(contents))
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var e entry
		if _, err := fmt.Sscanf(s.Text(), "%d %d", &e.off, &e.length); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

// prefault installs pages ahead of the guest asking for them.
//
// Runs concurrently with the serve loop rather than before it. UFFDIO_COPY is
// idempotent -- a page another worker already installed comes back EEXIST --
// so racing is safe, and doing it first would block Firecracker's resume for
// as long as the replay takes.
// extra is appended AFTER the recorded order. The recorded order is a
// SEQUENCE whose value is that it matches the guest's real access order; extra
// is a SET with no ordering, so putting it first would delay the pages the
// guest is about to ask for. Duplicates are free: replay is idempotent and an
// already-installed page answers EEXIST, which counts as success.
func prefault(ctx context.Context, h *handshake, src block.Slicer,
	contents []byte, extra []entry, stats *Stats) {

	// One bulk fetch first, always, list or no list.
	//
	// This is the anti-fault-storm guarantee, and it is not an optimisation.
	// Without it every page is its own round trip to object storage: at ~50ms
	// each, a 256MiB guest takes over an hour to page in. With it, the whole
	// packed image lands in one request and every fault afterwards is a local
	// read.
	start := time.Now()
	if err := src.Prefault(ctx); err != nil {
		slog.Warn("uffd bulk fetch failed; falling back to per-page reads", "err", err)
	} else {
		slog.Info("uffd bulk fetch done", "ms", time.Since(start).Milliseconds())
	}

	entries := append(parsePrefetch(contents), extra...)
	if len(entries) == 0 {
		return
	}
	replay(ctx, h, src, entries, stats)
}

// replay installs every recorded page, fetching and copying in parallel.
func replay(ctx context.Context, h *handshake, src block.Slicer,
	entries []entry, stats *Stats) {

	type fetched struct{ off int64 }

	work := make(chan entry, len(entries))
	for _, e := range entries {
		work <- e
	}
	close(work)

	pages := make(chan fetched, prefetchFetchWorkers*4)
	var copied, skipped, late atomic.Int64
	start := time.Now()

	var fetchWg sync.WaitGroup
	for w := 0; w < prefetchFetchWorkers; w++ {
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
			for e := range work {
				if ctx.Err() != nil {
					return
				}
				// Warm the local cache for this range; the copy worker
				// below does the authoritative read into its own page
				// buffer, because filling a whole guest page can take
				// several Slice calls (see fillPage).
				if _, err := src.Slice(ctx, e.off, e.length); err != nil {
					skipped.Add(1)
					continue
				}
				select {
				case pages <- fetched{off: e.off}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	var copyWg sync.WaitGroup
	for w := 0; w < prefetchCopyWorkers; w++ {
		copyWg.Add(1)
		go func() {
			defer copyWg.Done()
			buf := make([]byte, h.pageSize)
			for p := range pages {
				if ctx.Err() != nil {
					return
				}
				addr, ok := addrOf(h.regions, p.off)
				if !ok {
					skipped.Add(1)
					continue
				}
				// The SAME fill the demand path uses. This was a single
				// Slice plus a zero-fill of the remainder, which is correct
				// only while one block covers one page: at 2MiB it installed
				// 4KiB of real data and ~2MB of zeros over the guest's
				// memory, and the machine never came back from a restore.
				if err := fillPage(ctx, src, p.off, buf); err != nil {
					skipped.Add(1)
					continue
				}
				// Best-effort by design: a page that fails here is simply
				// served on demand later. It must NOT count toward the fault
				// stats, which exist to show what the guest actually needed.
				existed, err := installPage(h.uffd, addr, buf, h.pageSize, stats)
				if err != nil {
					skipped.Add(1)
					continue
				}
				if existed {
					// The guest faulted this page before the replay reached
					// it, so it was served on demand anyway and the replay
					// saved nothing. Counted as a miss.
					late.Add(1)
					continue
				}
				copied.Add(1)
			}
		}()
	}

	fetchWg.Wait()
	close(pages)
	copyWg.Wait()

	// Counted here rather than per page: what a scrape wants is how many
	// pages this replay put in ahead of demand, and installAll's own prefault
	// goes through the same function, so both are visible as replay work.
	// Replayed counts every page the replay put in ahead of demand; Hit counts
	// those the guest had not already asked for, which is the same set here.
	// The difference between them and `late` is the prediction's accuracy.
	stats.Replayed.Add(copied.Load() + late.Load())
	stats.PrefetchHit.Add(copied.Load())

	slog.Info("uffd prefetch replay done",
		"entries", len(entries), "installed", copied.Load(),
		"guest_got_there_first", late.Load(), "skipped", skipped.Load(),
		"ms", time.Since(start).Milliseconds())
}
