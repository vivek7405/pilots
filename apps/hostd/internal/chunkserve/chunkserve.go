// Package chunkserve lets a machine's memory and disk handlers read their
// builds without holding a storage credential.
//
// # The problem
//
// A handler is a separate process, spawned per machine, that reads build
// chunks out of object storage on demand. It got there by inheriting hostd's
// whole environment, PILOT_S3_ACCESS_KEY and all. So every machine on a host
// has, beside it, a process holding credentials for the bucket that contains
// EVERY tenant's builds, checkpoints and volume metadata. Nothing about the
// handler's job needs that: it reads a handful of named objects belonging to
// the one machine it serves.
//
// The handler is not the guest, and a guest does not get to run code in it.
// But it is the process closest to the guest, it parses bytes the guest's disk
// produced, and it is exactly the process an attacker who found a bug in the
// block layer would already be running inside. Credentials sitting in its
// environment turn that bug from "read this machine's disk" into "read the
// fleet's bucket", which is the difference this package exists to remove.
//
// # The shape
//
// hostd listens on a unix socket inside the machine's own state directory and
// answers two calls, Get and GetRange, which is the whole of the
// block.ObjectStore interface the handler needs. The handler is given the
// socket path and no credentials at all. Every request names a build id, and
// the server answers only for the ids that machine was spawned with: its
// memory build, that build's parent, the template build, and the rehydrate
// build. Anything else is refused and logged with the machine id, which is the
// line that makes a compromised handler visible rather than merely contained.
//
// The protocol is deliberately tiny: one JSON request per line, one length-
// prefixed response. Not gRPC, not HTTP, because this is a host-local socket
// between two processes of the same binary, and every dependency added here is
// one more thing a self-hosted install has to carry.
package chunkserve

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// SocketName is the socket's name inside a machine's state directory.
const SocketName = "chunks.sock"

// SocketPath is where a machine's chunk socket lives.
func SocketPath(stateDir string) string { return filepath.Join(stateDir, SocketName) }

// request is one call. Length is zero for a whole-object Get.
type request struct {
	Key    string `json:"key"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
	Range  bool   `json:"range,omitempty"`
}

// response header codes. The error text is carried rather than the Go error,
// because the client has to reconstruct ErrRangeNotSatisfiable specifically:
// a zero-length data object answers every range with 416, and that means
// "these blocks are zeros", not "this read failed".
const (
	codeOK       = 0
	codeError    = 1
	codeNotRange = 2
	// codeRefused is its own code rather than an error string, so the client
	// can tell "you may not read this" from "the socket hiccupped" and not
	// retry the one that will never change.
	codeRefused = 3
)

// Server answers chunk reads for ONE machine.
type Server struct {
	machineID string
	store     block.ObjectStore
	allowed   map[string]bool
	ln        net.Listener

	mu      sync.Mutex
	refused int
}

// New starts a server on path, answering only for the given build ids.
//
// The allowlist is the point. A handler that asked for another machine's build
// would be answered by a plain proxy, which would make this package a
// credential with extra steps.
func New(machineID, path string, store block.ObjectStore, allowedBuildIDs []string) (*Server, error) {
	if store == nil {
		return nil, errors.New("chunkserve: no object store")
	}
	// A stale socket from a handler that died with the host is not an error:
	// the path is inside the machine's own state directory, so nothing else
	// can own it.
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("chunkserve: listen on %s: %w", path, err)
	}
	// Owner only. The handler runs as the same user hostd does; nothing else
	// on the host has any business reading a tenant's build.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	allowed := make(map[string]bool, len(allowedBuildIDs))
	for _, id := range allowedBuildIDs {
		if id != "" {
			allowed[id] = true
		}
	}

	s := &Server{machineID: machineID, store: store, allowed: allowed, ln: ln}
	go s.serve()
	return s, nil
}

// Allow adds a build id after the fact, for a machine that learns of one
// while running (a rehydrate that resolves late).
func (s *Server) Allow(buildID string) {
	if buildID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowed[buildID] = true
}

// Refused reports how many requests named a build this machine may not read.
// Above zero on a healthy host is a bug or an intrusion, never routine.
func (s *Server) Refused() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// Close stops the server and removes its socket.
func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	addr := s.ln.Addr().String()
	err := s.ln.Close()
	_ = os.Remove(addr)
	return err
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = writeError(conn, "chunkserve: unreadable request")
			return
		}
		if !s.permits(req.Key) {
			s.mu.Lock()
			s.refused++
			s.mu.Unlock()
			// Logged with the machine id, because this is the line that turns
			// a contained compromise into a visible one.
			slog.Warn("a chunk handler asked for a build it was not spawned for",
				"machine", s.machineID, "key", req.Key)
			if err := writeFrame(conn, codeRefused, []byte("chunkserve: this machine may not read "+req.Key)); err != nil {
				return
			}
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		var body []byte
		var readErr error
		if req.Range {
			body, readErr = s.store.GetRange(ctx, req.Key, req.Offset, req.Length)
		} else {
			body, readErr = s.store.Get(ctx, req.Key)
		}
		cancel()

		if readErr != nil {
			code := codeError
			if errors.Is(readErr, block.ErrRangeNotSatisfiable) {
				code = codeNotRange
			}
			if err := writeFrame(conn, byte(code), []byte(readErr.Error())); err != nil {
				return
			}
			continue
		}
		if err := writeFrame(conn, codeOK, body); err != nil {
			return
		}
	}
}

// permits reports whether a key belongs to a build this machine may read.
//
// A key is "<build id>/<something>", so the check is on the first segment
// rather than on the whole string: a build's header, data and index are
// separate objects under one id, and listing each would mean this package
// knowing the block layer's file names.
func (s *Server) permits(key string) bool {
	if key == "" || strings.Contains(key, "..") {
		return false
	}
	id, _, _ := strings.Cut(key, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowed[id]
}

func writeError(w io.Writer, msg string) error {
	return writeFrame(w, codeError, []byte(msg))
}

func writeFrame(w io.Writer, code byte, body []byte) error {
	var head [5]byte
	head[0] = code
	binary.BigEndian.PutUint32(head[1:], uint32(len(body)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}
