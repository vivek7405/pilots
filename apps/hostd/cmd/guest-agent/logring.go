package main

import (
	"io"
	"sync"
)

// A bounded, in-memory log buffer per process.
//
// In MEMORY on purpose, and this is the one decision worth defending. A file
// under /var/log would be simpler, and it would land in every snapshot: a
// machine's disk is chunked and uploaded, so an application that logs steadily
// would grow what every checkpoint and every restore has to move, forever, for
// data nobody reads twice. The ring costs a fixed megabyte of guest memory,
// which a snapshot pages in lazily, and it is gone when the machine is.
//
// The console mirror stays either way: everything written here is also written
// to the agent's stdout, which is the serial console the host already records.
// So this adds a way to read ONE process's output without taking anything
// away, which matters the moment a machine runs more than one.

// logRingSize is what one process may hold. A megabyte is a few thousand lines
// of ordinary application output: enough to see why something crashed, small
// enough that ten processes cost ten megabytes of a guest's memory.
const logRingSize = 1 << 20

// logRing is a fixed-size circular byte buffer. Oldest bytes are overwritten,
// so a chatty process cannot push the machine into swap.
type logRing struct {
	mu    sync.Mutex
	buf   []byte
	size  int
	start int
	full  bool
}

func newLogRing() *logRing {
	return &logRing{buf: make([]byte, 0, 4096)}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Grow up to the cap, then switch to overwriting. Starting small keeps an
	// idle machine's ten processes from reserving ten megabytes they never use.
	if !r.full && len(r.buf) < logRingSize {
		room := logRingSize - len(r.buf)
		take := p
		if len(take) > room {
			take = p[:room]
		}
		r.buf = append(r.buf, take...)
		p = p[len(take):]
		if len(r.buf) == logRingSize {
			r.full, r.size, r.start = true, logRingSize, 0
		}
		if len(p) == 0 {
			return len(take) + len(p), nil
		}
	}

	// Past the cap: write into the circle. A single write longer than the
	// whole ring keeps only its tail, which is the part that says what
	// happened last.
	if len(p) >= logRingSize {
		copy(r.buf, p[len(p)-logRingSize:])
		r.start = 0
		return len(p), nil
	}
	for _, b := range p {
		r.buf[r.start] = b
		r.start = (r.start + 1) % logRingSize
	}
	return len(p), nil
}

// Bytes returns the buffered output, oldest first.
func (r *logRing) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return out
	}
	out := make([]byte, 0, logRingSize)
	out = append(out, r.buf[r.start:]...)
	out = append(out, r.buf[:r.start]...)
	return out
}

// Tail returns the last n lines, or everything when n is zero or larger than
// what is held.
func (r *logRing) Tail(n int) []byte {
	all := r.Bytes()
	if n <= 0 {
		return all
	}
	count, cut := 0, 0
	for i := len(all) - 1; i >= 0; i-- {
		if all[i] != '\n' || i == len(all)-1 {
			continue
		}
		count++
		if count == n {
			cut = i + 1
			break
		}
	}
	return all[cut:]
}

// teeTo copies from src into the ring and to mirror, until src ends.
//
// Both, not either: the ring answers "show me this process", the mirror keeps
// the serial console that the host's machine-level log already reads. A
// process whose output only reached the ring would vanish from `pilot logs`.
func teeTo(ring *logRing, mirror io.Writer, src io.Reader) {
	_, _ = io.Copy(io.MultiWriter(ring, mirror), src)
}
