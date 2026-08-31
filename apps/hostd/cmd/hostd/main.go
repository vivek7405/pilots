// Command hostd is the per-host data plane.
//
// Every host in the fleet runs an identical copy: there is no control-plane
// tier and no scheduler. A host serves the full public API, routes and wakes
// its own machines, and reads cluster state from a local replica so that no
// request path depends on another host being alive.
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
	"strings"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/router"
	"github.com/vivek7405/pilots/hostd/internal/s3"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// shutdownTimeout bounds a graceful stop.
//
// SIGTERM DETACHES: the machines keep running and are re-adopted on the next
// start. A restart of the daemon must not be an outage for the workloads it
// happens to be supervising, so the systemd unit uses KillMode=process and
// this path only drains HTTP.
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

	for _, dir := range []string{
		filepath.Dir(cfg.StateDSN),
		cfg.MachineStateRoot(),
		cfg.CacheRoot(),
		cfg.ChrootBase,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	store, err := state.Open(cfg.StateDSN)
	if err != nil {
		return err
	}
	defer store.Close()

	uploader, err := newUploader(cfg)
	if err != nil {
		return err
	}

	mgr := machines.New(machines.Options{
		HostID:    cfg.HostID,
		Domain:    cfg.WorkloadDomain,
		StateRoot: cfg.MachineStateRoot(),
		CacheRoot: cfg.CacheRoot(),
		Store:     store,
		Uploader:  uploader,
		FCConfig: fc.Config{
			KernelPath:     cfg.KernelPath,
			TemplateRootfs: cfg.TemplateRootfs,
			FirecrackerBin: cfg.FirecrackerBin,
			JailerBin:      cfg.JailerBin,
			ChrootBase:     cfg.ChrootBase,
			CPUTemplate:    cfg.CPUTemplate,
			JailUID:        cfg.JailUID,
			JailGID:        cfg.JailGID,
			Limits:         fc.Limits{PidsMax: 2048},
		},
	})

	// Re-adopt machines that outlived the previous hostd. This is what makes a
	// restart safe: the processes are still serving, and picking them back up
	// costs nothing.
	adopted := reconcile(cfg, mgr)
	if adopted > 0 {
		slog.Info("re-adopted machines from a previous run", "count", adopted)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go mgr.RunIdleMonitor(ctx)
	// Sweeps up Firecrackers this host has no record of -- the residue of a
	// hostd killed mid-create, or a destroy that failed partway.
	go mgr.RunReaper(ctx)

	rtr := router.New(router.Options{
		Domain:  cfg.WorkloadDomain,
		Store:   store,
		Manager: mgr,
		SlotFor: mgr.SlotFor,
	})

	// One listener, two audiences: requests for a workload hostname are
	// proxied into a machine, everything else is the control API. Keeping them
	// on one port means a host needs exactly one address to be useful.
	handler := dispatch(cfg, rtr, api.Routes(api.Deps{
		HostID: cfg.HostID, Store: store, Machines: mgr,
	}))

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: exec streams, log follows and build logs are all
		// long-lived by design.
	}

	// Bind before announcing readiness. Doing this inside the serving goroutine
	// would let notifyReady() fire while the bind was still failing, and under
	// Type=notify systemd would consider the host up while it served nothing.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	slog.Info("hostd listening",
		"addr", ln.Addr().String(), "host_id", cfg.HostID, "domain", cfg.WorkloadDomain)
	notifyReady()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// Machines are deliberately left running.
		slog.Info("draining; machines stay up and will be re-adopted", "timeout", shutdownTimeout)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// dispatch sends workload hostnames to the router and everything else to the
// control API.
func dispatch(cfg *config.Config, rtr http.Handler, ctrl http.Handler) http.Handler {
	suffix := "." + strings.ToLower(cfg.WorkloadDomain)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.HasSuffix(host, suffix) {
			rtr.ServeHTTP(w, r)
			return
		}
		ctrl.ServeHTTP(w, r)
	})
}

// reconcile re-adopts machines left running by a previous hostd.
func reconcile(cfg *config.Config, mgr *machines.Manager) int {
	found, err := fc.Reconcile(cfg.MachineStateRoot())
	if err != nil {
		slog.Error("reconcile failed", "err", err)
		return 0
	}

	var adopted int
	for _, r := range found {
		if !r.Alive {
			// The process is gone; clear the breadcrumbs so the next start
			// does not keep trying to adopt a machine that no longer exists.
			_ = fc.ClearBreadcrumbs(filepath.Join(cfg.MachineStateRoot(), r.State.MachineID))
			continue
		}
		m := fc.Adopted(r.State, cfg.MachineStateRoot())
		if err := mgr.Adopt(r.State.MachineID, m, r.State.SlotIdx); err != nil {
			slog.Error("could not adopt machine", "machine", r.State.MachineID, "err", err)
			continue
		}
		adopted++
	}
	return adopted
}

func newUploader(cfg *config.Config) (fc.Uploader, error) {
	if cfg.S3Bucket == "" {
		// A stub, not nil. Returning a nil interface meant the first idle
		// suspend dereferenced it in the idle monitor's own goroutine and took
		// the whole daemon down about a minute after the first machine went
		// quiet -- and Restart=always then crash-looped it. The failure belongs
		// on the operation, not on the process.
		slog.Warn("no object storage configured; suspend and restore will fail " +
			"until PILOT_S3_BUCKET and credentials are set")
		return fc.UnconfiguredStore{}, nil
	}
	return s3.New(context.Background(), s3.Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
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
