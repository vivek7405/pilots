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

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// Client is the handler's side: a block.ObjectStore that holds no credential
// and can reach nothing but its own machine's builds.
//
// One connection, serialized by a mutex, because a handler's reads are already
// serialized by the kernel's block queue and a pool here would buy nothing but
// a way to leak descriptors. The connection is reopened on demand, so a hostd
// restart that takes the socket with it costs one failed read rather than a
// wedged machine.
type Client struct {
	path string

	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// Dial returns a client for the socket at path. It does not connect yet: the
// handler is spawned before the first read, and failing here would turn a
// transient socket race into a machine that will not start.
func Dial(path string) *Client { return &Client{path: path} }

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.r = nil, nil
	return err
}

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	return c.call(ctx, request{Key: key})
}

func (c *Client) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	return c.call(ctx, request{Key: key, Offset: offset, Length: length, Range: true})
}

func (c *Client) call(ctx context.Context, req request) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	body, err := c.roundTrip(ctx, req)
	if err != nil && !errors.Is(err, block.ErrRangeNotSatisfiable) && !isRefusal(err) {
		// One retry on a transport failure, with a fresh connection: hostd may
		// have restarted, and the handler outlives it by design.
		c.dropLocked()
		body, err = c.roundTrip(ctx, req)
	}
	return body, err
}

func (c *Client) roundTrip(ctx context.Context, req request) ([]byte, error) {
	if err := c.connectLocked(ctx); err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("chunkserve: write: %w", err)
	}

	var head [5]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return nil, fmt.Errorf("chunkserve: read header: %w", err)
	}
	n := binary.BigEndian.Uint32(head[1:])
	out := make([]byte, n)
	if _, err := io.ReadFull(c.r, out); err != nil {
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

func (c *Client) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return fmt.Errorf("chunkserve: dial %s: %w", c.path, err)
	}
	c.conn, c.r = conn, bufio.NewReader(conn)
	return nil
}

func (c *Client) dropLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn, c.r = nil, nil
}

// isRefusal reports an answer that will not change on a retry.
func isRefusal(err error) bool {
	return err != nil && errors.Is(err, errRefused)
}

var errRefused = errors.New("chunkserve: refused")
