package uffd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/ctlsock"
)

// acceptTimeout bounds the wait for Firecracker to connect. It has to outlast
// a slow snapshot load, but a handler that waits forever for a Firecracker
// that died is a process nothing will ever reap.
const acceptTimeout = 60 * time.Second

// Config describes one machine's memory to serve.
type Config struct {
	// Socket is where Firecracker connects. Its path goes into the snapshot
	// load request as mem_backend.backend_path.
	Socket string

	// MemFile reads a plain memory image. BuildID reads a chunked build from
	// object storage, with ParentBuildID resolving the ranges a diff did not
	// store. Exactly one of MemFile and BuildID.
	MemFile       string
	BuildID       uuid.UUID
	ParentBuildID uuid.UUID

	CacheRoot string

	// PrefetchFile is read for a fault order to replay and then rewritten with
	// this run's order. The same path on purpose: each wake refines the last.
	PrefetchFile string

	// ControlSock is where the handler answers hostd's requests. Optional: a
	// handler without one simply serves faults.
	ControlSock string

	// ReadyFD, when non-zero, receives a byte once the socket is listening.
	ReadyFD int
}

// Run serves one machine's memory until Firecracker exits or ctx is cancelled.
func Run(ctx context.Context, cfg Config, store block.ObjectStore) error {
	src, closeSrc, err := openSource(ctx, cfg, store)
	if err != nil {
		return err
	}
	defer closeSrc()

	// Read the replay list BEFORE the recorder truncates it. They are usually
	// the same file.
	prefetch := readPrefetch(cfg.PrefetchFile)

	ln, err := listen(cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()

	// Listening is what "ready" means here: Firecracker's snapshot load
	// connects to this socket, and a load that arrives first fails outright.
	signalReady(cfg.ReadyFD)

	h, err := accept(ln, acceptTimeout)
	if err != nil {
		return err
	}
	defer syscall.Close(h.uffd)

	rec, err := newRecorder(cfg.PrefetchFile)
	if err != nil {
		return err
	}
	defer rec.Close()

	var stats Stats

	if cfg.ControlSock != "" {
		ctl, err := ctlsock.Listen(cfg.ControlSock)
		if err != nil {
			return err
		}
		defer ctl.Close()
		go ctlsock.Serve(ctl, control(&prefaulter{h: h, src: src}, &stats))
	}

	go prefault(ctx, h, src, prefetch, &stats)

	start := time.Now()
	err = serve(ctx, h, src, &stats, rec)
	elapsed := time.Since(start)

	slog.Info("uffd handler exiting",
		"ms", elapsed.Milliseconds(),
		"faults", stats.Faults.Load(),
		"bytes", stats.BytesCopied.Load(),
		"eexist", stats.CopyEEXIST.Load(),
		"eagain", stats.CopyEAGAIN.Load(),
		"short", stats.CopyShort.Load(),
		"failed", stats.CopyFailed.Load(),
		"minor", stats.MinorFaults.Load(),
		"wp", stats.WPFaults.Load())

	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// openSource opens the memory image the faults are served from.
func openSource(ctx context.Context, cfg Config, store block.ObjectStore) (block.Slicer, func(), error) {
	switch {
	case cfg.MemFile != "" && cfg.BuildID != uuid.Nil:
		return nil, nil, errors.New("uffd: memfile and build-id are exclusive")

	case cfg.MemFile != "":
		s, err := block.OpenFileSlicer(cfg.MemFile, block.DefaultBlockSize)
		if err != nil {
			return nil, nil, err
		}
		return s, func() { s.Close() }, nil

	case cfg.BuildID != uuid.Nil:
		if store == nil {
			return nil, nil, errors.New("uffd: build-id needs object storage")
		}
		b, err := block.OpenRemoteBuild(ctx, store, cfg.BuildID, cfg.CacheRoot)
		if err != nil {
			return nil, nil, err
		}
		if cfg.ParentBuildID == uuid.Nil {
			return b, func() { b.Close() }, nil
		}

		parent, err := block.OpenRemoteBuild(ctx, store, cfg.ParentBuildID, cfg.CacheRoot)
		if err != nil {
			b.Close()
			return nil, nil, fmt.Errorf("uffd: open parent build: %w", err)
		}
		b.SetParent(parent)
		return b, func() { b.Close(); parent.Close() }, nil

	default:
		return nil, nil, errors.New("uffd: one of memfile or build-id is required")
	}
}

// listen creates the socket Firecracker connects to.
func listen(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("uffd: mkdir for socket: %w", err)
	}
	// A socket left by a handler that was SIGKILLed refuses to bind, which
	// would make a machine that died badly impossible to wake.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("uffd: clear socket %s: %w", path, err)
	}

	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("uffd: resolve %s: %w", path, err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("uffd: listen on %s: %w", path, err)
	}
	// Firecracker runs jailed as another uid and still has to connect.
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, fmt.Errorf("uffd: chmod socket: %w", err)
	}
	return ln, nil
}

func signalReady(fd int) {
	if fd <= 0 {
		return
	}
	f := os.NewFile(uintptr(fd), "uffd-ready")
	_, _ = f.Write([]byte("ready\n"))
	_ = f.Close()
}

// SocketFor is where a machine's handler listens.
func SocketFor(stateDir string) string { return filepath.Join(stateDir, "uffd.sock") }

// ControlSockFor is where a machine's fault handler answers hostd.
func ControlSockFor(stateDir string) string { return filepath.Join(stateDir, "uffd-ctl.sock") }

// PrefetchFor is where a machine's fault order is kept between wakes.
func PrefetchFor(stateDir string) string { return filepath.Join(stateDir, "prefetch.txt") }
