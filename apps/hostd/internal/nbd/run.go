package nbd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/ctlsock"
)

// Config describes one disk to serve.
type Config struct {
	// Device is the /dev/nbdN path; Index is its number.
	Device string
	Index  int

	// TemplateDir reads the template from a local build directory.
	// TemplateBuildID fetches it from object storage instead. Exactly one.
	TemplateDir     string
	TemplateBuildID uuid.UUID

	// CachePath is the machine's copy-on-write file. It is NEVER deleted by
	// the handler: it holds writes hostd still has to chunkify.
	CachePath string

	// RehydrateBuildID replays a previous lifetime's writes into the cache
	// before the first request is served.
	RehydrateBuildID uuid.UUID

	// CacheRoot backs remote builds.
	CacheRoot string

	// ControlSock is where the handler answers dirty/stop.
	ControlSock string

	// ReadyFD, when non-zero, receives a byte once the device is online.
	ReadyFD int

	ReadOnly bool
}

// Run serves one device until it is disconnected. It is the body of the
// handler process, not something hostd calls in-process.
func Run(ctx context.Context, cfg Config, store block.ObjectStore) error {
	template, closeTemplate, err := openTemplate(ctx, cfg, store)
	if err != nil {
		return err
	}
	defer closeTemplate()

	// Pull the whole template in one request, in the background, while the
	// guest is already being served. Left lazy, a boot is thousands of
	// round-trips to object storage; done in the foreground, nothing is served
	// until the last byte lands.
	go func() {
		start := time.Now()
		if err := template.Prefault(ctx); err != nil {
			slog.Warn("nbd template prefault failed", "err", err)
			return
		}
		slog.Info("nbd template prefaulted", "ms", time.Since(start).Milliseconds())
	}()

	cache, err := block.NewCache(template.Size(), template.BlockSize(), cfg.CachePath, false)
	if err != nil {
		return err
	}

	if cfg.RehydrateBuildID != uuid.Nil {
		if err := rehydrate(ctx, cfg, store, template, cache); err != nil {
			return err
		}
	}

	overlay := block.NewOverlay(template, cache)

	ln, err := ctlsock.Listen(cfg.ControlSock)
	if err != nil {
		return err
	}
	defer ln.Close()

	// stop is what the control channel's "stop" triggers. It disconnects the
	// device, which is what makes the serve loop's socket close and
	// NBD_DO_IT return.
	var once sync.Once
	stopped := make(chan struct{})
	stop := func() {
		once.Do(func() {
			_ = DisconnectDevice(cfg.Index)
			close(stopped)
		})
	}
	go ctlsock.Serve(ln, control(cache, stop))

	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-stopped:
		}
	}()

	ready := make(chan struct{})
	go func() {
		<-ready
		signalReady(cfg.ReadyFD)
	}()

	err = Serve(cfg.Index, overlay, cfg.ReadOnly, ready)

	// Deliberately NOT overlay.Close(). Cache.Close removes the backing file,
	// and that file is the machine's disk since its last snapshot -- hostd
	// chunkifies it after this process is gone. The kernel drops the mapping
	// on exit; the bytes stay.
	return err
}

// openTemplate opens the read-only base the overlay falls through to.
func openTemplate(ctx context.Context, cfg Config, store block.ObjectStore) (block.Slicer, func(), error) {
	switch {
	case cfg.TemplateDir != "" && cfg.TemplateBuildID != uuid.Nil:
		return nil, nil, fmt.Errorf("nbd: template-dir and template-build-id are exclusive")

	case cfg.TemplateDir != "":
		b, err := block.OpenLocalBuild(cfg.TemplateDir)
		if err != nil {
			return nil, nil, err
		}
		return b, func() { b.Close() }, nil

	case cfg.TemplateBuildID != uuid.Nil:
		if store == nil {
			return nil, nil, fmt.Errorf("nbd: template-build-id needs object storage")
		}
		b, err := block.OpenRemoteBuild(ctx, store, cfg.TemplateBuildID, cfg.CacheRoot)
		if err != nil {
			return nil, nil, err
		}
		return b, func() { b.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("nbd: one of template-dir or template-build-id is required")
	}
}

// rehydrate replays a previous lifetime's writes into the cache.
//
// It runs BEFORE the device accepts a single request. A guest that reads a
// block between attach and rehydration sees the template's version of it and
// caches that in its page cache -- and no later write fixes what it already
// believes.
func rehydrate(ctx context.Context, cfg Config, store block.ObjectStore,
	template block.Slicer, cache *block.Cache) error {

	if store == nil {
		return fmt.Errorf("nbd: rehydrate-build-id needs object storage")
	}
	start := time.Now()

	diff, err := block.OpenRemoteBuild(ctx, store, cfg.RehydrateBuildID, cfg.CacheRoot)
	if err != nil {
		return fmt.Errorf("nbd: open rehydrate build: %w", err)
	}
	defer diff.Close()
	diff.SetParent(template)

	if err := cache.PopulateFromSlicer(ctx, diff); err != nil {
		return fmt.Errorf("nbd: rehydrate: %w", err)
	}
	slog.Info("nbd cache rehydrated",
		"build", cfg.RehydrateBuildID, "ms", time.Since(start).Milliseconds())
	return nil
}

// signalReady tells the parent the device is online and safe to hand to
// Firecracker.
func signalReady(fd int) {
	if fd <= 0 {
		return
	}
	f := os.NewFile(uintptr(fd), "nbd-ready")
	_, _ = f.Write([]byte("ready\n"))
	_ = f.Close()
}

// CachePathFor is where a machine's copy-on-write file lives.
func CachePathFor(stateDir string) string {
	return filepath.Join(stateDir, "rootfs.cow")
}

// ControlSockFor is where a machine's handler answers control requests.
//
// Deliberately short: a unix socket path is capped at 108 bytes, and a machine
// state directory nested a few levels deeper would silently truncate.
func ControlSockFor(stateDir string) string {
	return filepath.Join(stateDir, "nbd.sock")
}
