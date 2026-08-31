// Command hostd is the per-host data plane.
//
// Every host in the fleet runs an identical copy: there is no control-plane
// tier and no scheduler. A host serves the full public API, routes and wakes
// its own machines, and reads cluster state from a local replica so that no
// request path depends on another host being alive.
//
// Phase 1 stands up the process, its configuration, its state store, and the
// API shapes. The engine -- Firecracker lifecycle, netns, router, snapshots --
// lands in Phase 2 (issue #3).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// shutdownTimeout bounds a graceful stop. A host that hangs on shutdown blocks
// its own restart and strands machines; the systemd unit's TimeoutStopSec is
// set higher than this so systemd never has to SIGKILL us first.
const shutdownTimeout = 30 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if err := run(); err != nil {
		slog.Error("hostd exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if dir := filepath.Dir(cfg.StateDSN); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	store, err := state.Open(cfg.StateDSN)
	if err != nil {
		return err
	}
	defer store.Close()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Routes(api.Deps{HostID: cfg.HostID, Store: store}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout on purpose: exec streams, log follows, and build log
		// streams are long-lived by design.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("hostd listening", "addr", cfg.ListenAddr, "host_id", cfg.HostID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	notifyReady()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down", "timeout", shutdownTimeout)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// notifyReady tells systemd (Type=notify) that the process is serving. Done by
// hand rather than with a dependency: it is one datagram on a unix socket.
func notifyReady() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	// A leading '@' denotes an abstract socket, written as a NUL byte.
	if sock[0] == '@' {
		sock = "\x00" + sock[1:]
	}
	conn, err := net.Dial("unixgram", sock)
	if err != nil {
		slog.Warn("sd_notify dial failed", "err", err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1\n")); err != nil {
		slog.Warn("sd_notify write failed", "err", err)
	}
}
