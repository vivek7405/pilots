package chunkserve

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowStore answers every read after a fixed delay, and records the highest
// number of reads that were ever in flight at once.
type slowStore struct {
	delay time.Duration

	mu     sync.Mutex
	live   int
	peak   int
	served atomic.Int64
}

func (s *slowStore) enter() {
	s.mu.Lock()
	s.live++
	if s.live > s.peak {
		s.peak = s.live
	}
	s.mu.Unlock()
}

func (s *slowStore) leave() {
	s.mu.Lock()
	s.live--
	s.mu.Unlock()
}

func (s *slowStore) Get(_ context.Context, _ string) ([]byte, error) {
	s.enter()
	defer s.leave()
	time.Sleep(s.delay)
	s.served.Add(1)
	return []byte("chunk"), nil
}

func (s *slowStore) GetRange(ctx context.Context, key string, _, _ int64) ([]byte, error) {
	return s.Get(ctx, key)
}

func (s *slowStore) peakLive() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// Concurrent reads are actually concurrent.
//
// The client held ONE connection behind a mutex, on the reasoning that "a
// handler's reads are already serialized by the kernel's block queue". That
// was never true of the reader that matters: uffd's prefetch replay runs
// sixteen fetchers, a number chosen precisely to hide per-request latency to
// object storage, and all sixteen called through here. The parallelism the
// replay was built around was cancelled by its transport, which is the resume
// gap that AGENTS.md bar 5 makes a regression in its own right.
//
// Measured rather than asserted structurally: sixteen reads of 40 ms each take
// about 40 ms through a pool and about 640 ms through one connection.
// Restoring the single connection reds both the peak and the elapsed check.
func TestConcurrentReadsAreNotSerialized(t *testing.T) {
	const readers = 16
	store := &slowStore{delay: 40 * time.Millisecond}
	dir := t.TempDir()
	srv, err := New("m_pool", filepath.Join(dir, SocketName), store, []string{"b1"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := Dial(filepath.Join(dir, SocketName))
	defer c.Close()

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(t.Context(), "b1/data"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if got := store.peakLive(); got < readers/2 {
		t.Errorf("at most %d reads were ever in flight out of %d; the pool is not "+
			"admitting concurrent readers", got, readers)
	}
	// Generous: serial would be 16 * 40ms = 640ms. Anything under half of that
	// proves the reads overlapped, without making the test a stopwatch.
	if elapsed > 320*time.Millisecond {
		t.Errorf("%d reads of %v took %v; serialized through one connection they "+
			"would take about %v, which is the resume gap this costs",
			readers, store.delay, elapsed, time.Duration(readers)*store.delay)
	}
}

// A connection is reused rather than reopened per read, so the pool does not
// trade serialisation for a dial on every chunk.
func TestAPooledConnectionIsReused(t *testing.T) {
	store := &slowStore{}
	dir := t.TempDir()
	srv, err := New("m_reuse", filepath.Join(dir, SocketName), store, []string{"b1"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := Dial(filepath.Join(dir, SocketName))
	defer c.Close()

	for i := 0; i < 20; i++ {
		if _, err := c.Get(t.Context(), "b1/data"); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	c.mu.Lock()
	open := c.open
	c.mu.Unlock()
	if open != 1 {
		t.Errorf("twenty sequential reads opened %d connections, want one reused", open)
	}
}

// A refusal leaves the connection usable: the server framed the answer, so
// nothing about the stream is in doubt, and discarding it would make an
// unauthorised key cost a reconnect per attempt.
func TestARefusalDoesNotDiscardTheConnection(t *testing.T) {
	store := &slowStore{}
	dir := t.TempDir()
	srv, err := New("m_refuse", filepath.Join(dir, SocketName), store, []string{"b1"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := Dial(filepath.Join(dir, SocketName))
	defer c.Close()

	if _, err := c.Get(t.Context(), "b1/data"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Get(t.Context(), "other/data"); err == nil {
		t.Fatal("a key from another build was served")
	}
	if _, err := c.Get(t.Context(), "b1/data"); err != nil {
		t.Fatalf("the connection was unusable after a refusal: %v", err)
	}
	c.mu.Lock()
	open := c.open
	c.mu.Unlock()
	if open != 1 {
		t.Errorf("open = %d, want one connection carried through the refusal", open)
	}
}

// The pool is bounded. The descriptor worry behind the original single
// connection was fair, so more readers than slots wait rather than opening
// without limit.
func TestThePoolIsBounded(t *testing.T) {
	store := &slowStore{delay: 5 * time.Millisecond}
	dir := t.TempDir()
	srv, err := New("m_bound", filepath.Join(dir, SocketName), store, []string{"b1"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := Dial(filepath.Join(dir, SocketName))
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < maxConns*3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Get(t.Context(), "b1/data")
		}()
	}
	wg.Wait()

	c.mu.Lock()
	open := c.open
	c.mu.Unlock()
	if open > maxConns {
		t.Errorf("open = %d, want at most %d", open, maxConns)
	}
	if store.served.Load() != int64(maxConns*3) {
		t.Errorf("served %d of %d reads", store.served.Load(), maxConns*3)
	}
}
