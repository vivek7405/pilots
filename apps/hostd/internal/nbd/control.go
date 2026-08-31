package nbd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/vivek7405/pilots/hostd/internal/block"
)

// The handler runs as its own process, so hostd can be restarted -- or crash
// -- without wedging every running VM's disk. That isolation costs one thing:
// the dirty-block bitmap lives in the child's memory, and a checkpoint needs
// it in the parent's.
//
// Deriving it from the file instead is not an option. The cache is sparse, but
// filesystems allocate in extents larger than a block, so SEEK_DATA reports
// blocks that were never written. Recording one of those as "mine" writes
// zeros over the template's content, which is corruption a test would only
// catch by reading the exact wrong block.
//
// Hence a control socket: a line command, a length, and the bytes.
const (
	cmdDirty = "dirty"
	cmdStop  = "stop"

	controlTimeout = 15 * time.Second
)

// controlServer answers the parent's requests inside the handler process.
type controlServer struct {
	listener net.Listener
	cache    *block.Cache
	stop     func()
}

// serveControl accepts control connections until the listener closes.
func serveControl(ln net.Listener, cache *block.Cache, stop func()) {
	s := &controlServer{listener: ln, cache: cache, stop: stop}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // the listener was closed: the handler is shutting down
		}
		s.handle(conn)
		conn.Close()
	}
}

func (s *controlServer) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}

	switch strings.TrimSpace(line) {
	case cmdDirty:
		// Sync first. The bitmap describes blocks the mapping holds, and a
		// parent that chunkifies the file before those pages reach it would
		// store zeros for blocks the bitmap swears are written -- the exact
		// corruption the bitmap exists to prevent, arrived at from the other
		// side.
		if err := s.cache.Sync(); err != nil {
			writeErr(conn, err)
			return
		}
		var buf bytes.Buffer
		if _, err := s.cache.Dirty().WriteTo(&buf); err != nil {
			writeErr(conn, err)
			return
		}
		fmt.Fprintf(conn, "OK %d\n", buf.Len())
		_, _ = conn.Write(buf.Bytes())

	case cmdStop:
		fmt.Fprint(conn, "OK 0\n")
		s.stop()

	default:
		writeErr(conn, fmt.Errorf("unknown command %q", strings.TrimSpace(line)))
	}
}

func writeErr(conn net.Conn, err error) {
	// Newlines would be read as the end of the status line.
	fmt.Fprintf(conn, "ERR %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
}

// control sends one command to a handler and returns its payload.
func control(sock, cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", sock, controlTimeout)
	if err != nil {
		return nil, fmt.Errorf("nbd: dial control socket %s: %w", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))

	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return nil, fmt.Errorf("nbd: send %s: %w", cmd, err)
	}

	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("nbd: read %s status: %w", cmd, err)
	}
	status = strings.TrimSpace(status)

	if rest, ok := strings.CutPrefix(status, "ERR "); ok {
		return nil, fmt.Errorf("nbd: handler refused %s: %s", cmd, rest)
	}
	rest, ok := strings.CutPrefix(status, "OK ")
	if !ok {
		return nil, fmt.Errorf("nbd: malformed %s response %q", cmd, status)
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return nil, fmt.Errorf("nbd: malformed %s length %q", cmd, rest)
	}
	if n == 0 {
		return nil, nil
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("nbd: read %s payload: %w", cmd, err)
	}
	return payload, nil
}

// listenControl creates the handler's control socket.
func listenControl(path string) (net.Listener, error) {
	// A stale socket from a previous lifetime refuses to bind. Removing it is
	// safe here because the handler owns the path for the life of one machine
	// and a live handler would still be holding the device, not the file.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("nbd: clear control socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("nbd: listen on %s: %w", path, err)
	}
	return ln, nil
}

// parseDirty decodes a bitmap from the wire.
func parseDirty(payload []byte) (*roaring.Bitmap, error) {
	bm := roaring.New()
	if len(payload) == 0 {
		return bm, nil
	}
	if _, err := bm.ReadFrom(bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("nbd: decode dirty bitmap: %w", err)
	}
	return bm, nil
}
