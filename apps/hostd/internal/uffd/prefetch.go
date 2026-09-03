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
)

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
func prefault(ctx context.Context, h *handshake, src block.Slicer,
	contents []byte, stats *Stats) {

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

	entries := parsePrefetch(contents)
	if len(entries) == 0 {
		return
	}
	replay(ctx, h, src, entries, stats)
}

// replay installs every recorded page, fetching and copying in parallel.
func replay(ctx context.Context, h *handshake, src block.Slicer,
	entries []entry, stats *Stats) {

	type fetched struct {
		off  int64
		data []byte
	}

	work := make(chan entry, len(entries))
	for _, e := range entries {
		work <- e
	}
	close(work)

	pages := make(chan fetched, prefetchFetchWorkers*4)
	var copied, skipped atomic.Int64
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
				data, err := src.Slice(ctx, e.off, e.length)
				if err != nil {
					skipped.Add(1)
					continue
				}
				select {
				case pages <- fetched{off: e.off, data: data}:
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
				for i := copy(buf, p.data); i < len(buf); i++ {
					buf[i] = 0
				}
				// Best-effort by design: a page that fails here is simply
				// served on demand later. It must NOT count toward the fault
				// stats, which exist to show what the guest actually needed.
				if err := installPage(h.uffd, addr, buf, h.pageSize, stats); err != nil {
					skipped.Add(1)
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
	stats.Replayed.Add(copied.Load())

	slog.Info("uffd prefetch replay done",
		"entries", len(entries), "installed", copied.Load(), "skipped", skipped.Load(),
		"ms", time.Since(start).Milliseconds())
}
