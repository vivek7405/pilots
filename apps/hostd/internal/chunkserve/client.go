package chunkserve

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/block"
)

// Client is the handler's side: a block.ObjectStore that holds no credential
// and can reach nothing but its own machine's builds.
//
// A POOL of connections, not one.
//
// It was one, serialized by a mutex, on the reasoning that "a handler's reads
// are already serialized by the kernel's block queue". That was never true of
// the reader that matters. The uffd prefetch replay runs prefetchFetchWorkers
// fetchers -- sixteen of them, a number chosen precisely to hide per-request
// latency to object storage -- and every one of them called through here, so
// sixteen concurrent fetches became one at a time and the parallelism the
// replay was built around was cancelled by its transport. That is the resume
// gap, which AGENTS.md bar 5 makes a regression in its own right.
//
// Worse than slow: a DEMAND fault, the one a guest is stalled on right now,
// queued behind whatever prefetch read held the mutex. A background read
// blocking a foreground one is head-of-line blocking in the place it costs
// most.
//
// Bounded, because the original worry about descriptors was fair. maxConns is
// above the prefetch worker count on purpose: a pool no larger than the
// background readers would leave a demand fault waiting again.
//
// Connections are reopened on demand, so a hostd restart that takes the socket
// with it costs one failed read rather than a wedged machine.
type Client struct {
	path string

	mu   sync.Mutex
	idle []*conn
	open int
	free chan struct{} // one slot per connection this client may hold
	shut bool
}

// conn is one connection and the reader framing it.
type conn struct {
	c net.Conn
	r *bufio.Reader
}

// maxConns bounds how many connections one handler holds.
//
// Above uffd's prefetchFetchWorkers (16) with room for its copy workers and
// for demand faults, so the fault a guest is stalled on never waits behind the
// background replay. Not derived from that constant: this package does not
// import uffd, and a number that moves when an unrelated package is tuned is
// worse than one written down with its reason.
const maxConns = 32

// Dial returns a client for the socket at path. It does not connect yet: the
// handler is spawned before the first read, and failing here would turn a
// transient socket race into a machine that will not start.
func Dial(path string) *Client {
	c := &Client{path: path, free: make(chan struct{}, maxConns)}
	for i := 0; i < maxConns; i++ {
		c.free <- struct{}{}
	}
	return c
}

func (c *Client) Close() error {
	c.mu.Lock()
	idle := c.idle
	c.idle, c.open, c.shut = nil, 0, true
	c.mu.Unlock()

	var err error
	for _, cn := range idle {
		if e := cn.c.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// acquire takes a connection from the pool, opening one if the pool has room.
//
// Blocks when every slot is in use, which is the backpressure the single
// mutex used to provide -- except that it now admits maxConns readers at once
// instead of one.
func (c *Client) acquire(ctx context.Context) (*conn, error) {
	select {
	case <-c.free:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.mu.Lock()
	if c.shut {
		c.mu.Unlock()
		c.free <- struct{}{}
		return nil, errors.New("chunkserve: client is closed")
	}
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.open++
	c.mu.Unlock()

	cn, err := c.dial(ctx)
	if err != nil {
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
		c.free <- struct{}{}
		return nil, err
	}
	return cn, nil
}

// release returns a connection to the pool, or discards it when it is spent.
func (c *Client) release(cn *conn, keep bool) {
	c.mu.Lock()
	if keep && !c.shut {
		c.idle = append(c.idle, cn)
		c.mu.Unlock()
		c.free <- struct{}{}
		return
	}
	c.open--
	c.mu.Unlock()
	_ = cn.c.Close()
	c.free <- struct{}{}
}

func (c *Client) dial(ctx context.Context) (*conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, fmt.Errorf("chunkserve: dial %s: %w", c.path, err)
	}
	return &conn{c: nc, r: bufio.NewReader(nc)}, nil
}

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	return c.call(ctx, request{Key: key})
}

func (c *Client) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	return c.call(ctx, request{Key: key, Offset: offset, Length: length, Range: true})
}

func (c *Client) call(ctx context.Context, req request) ([]byte, error) {
	body, err := c.attempt(ctx, req)
	if err != nil && !errors.Is(err, block.ErrRangeNotSatisfiable) && !isRefusal(err) &&
		ctx.Err() == nil {
		// One retry on a transport failure, with a fresh connection: hostd may
		// have restarted, and the handler outlives it by design. The spent
		// connection was already discarded rather than returned to the pool,
		// so the retry cannot land on it.
		body, err = c.attempt(ctx, req)
	}
	return body, err
}

// attempt is one round trip on one pooled connection.
//
// A connection that produced a transport error is DISCARDED rather than
// returned: its framing is no longer trustworthy, and handing it to the next
// reader would turn one failed read into a stream of them. An answer the
// server framed correctly -- including a refusal or a 416 -- leaves the
// connection reusable, which is what keeps the pool from churning.
func (c *Client) attempt(ctx context.Context, req request) ([]byte, error) {
	cn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	body, err := c.roundTrip(ctx, cn, req)
	framed := err == nil || errors.Is(err, block.ErrRangeNotSatisfiable) || isRefusal(err)
	c.release(cn, framed)
	return body, err
}

func (c *Client) roundTrip(ctx context.Context, cn *conn, req request) ([]byte, error) {
	// Cleared as well as set. A pooled connection outlives the request that
	// set a deadline on it, and a stale absolute deadline would fail the NEXT
	// reader instantly for a reason nothing in its own context explains.
	if dl, ok := ctx.Deadline(); ok {
		_ = cn.c.SetDeadline(dl)
		defer func() { _ = cn.c.SetDeadline(time.Time{}) }()
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := cn.c.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("chunkserve: write: %w", err)
	}

	var head [5]byte
	if _, err := io.ReadFull(cn.r, head[:]); err != nil {
		return nil, fmt.Errorf("chunkserve: read header: %w", err)
	}
	n := binary.BigEndian.Uint32(head[1:])
	out := make([]byte, n)
	if _, err := io.ReadFull(cn.r, out); err != nil {
		return nil, fmt.Errorf("chunkserve: read body: %w", err)
	}
	switch head[0] {
	case codeOK:
		return out, nil
	case codeRefused:
		return nil, fmt.Errorf("%s: %w", out, errRefused)
	case codeNotRange:
		// Reconstructed rather than carried as text, because the block layer
		// branches on it: a zero-length data object answers every range with
		// 416, and that means "these blocks are zeros", not a failure.
		return nil, fmt.Errorf("%s: %w", out, block.ErrRangeNotSatisfiable)
	default:
		return nil, errors.New(string(out))
	}
}

// isRefusal reports an answer that will not change on a retry.
func isRefusal(err error) bool {
	return err != nil && errors.Is(err, errRefused)
}

var errRefused = errors.New("chunkserve: refused")
