package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/s3"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// runNBDHandler is hostd re-executed as a block-device server for one machine.
//
// A hidden subcommand rather than a second binary: it is the same code, and
// shipping it separately means a second artifact to build, version, and locate
// at runtime -- the predecessor guessed between two hard-coded paths and
// silently ran without copy-on-write when neither existed.
//
// It is a separate PROCESS, though, and that part is not negotiable.
// Firecracker blocks in uninterruptible sleep on a device whose server has
// gone away, so serving from inside hostd would turn every daemon restart into
// an outage for every running machine.
func runNBDHandler(args []string) error {
	fs := flag.NewFlagSet(nbd.SubcommandName, flag.ContinueOnError)

	device := fs.String("device", "", "block device to serve, e.g. /dev/nbd0")
	index := fs.Int("index", -1, "the device's number")
	cachePath := fs.String("cache", "", "the machine's copy-on-write file")
	controlSock := fs.String("control", "", "unix socket for dirty/stop")
	templateDir := fs.String("template-dir", "", "local template build directory")
	templateBuildID := fs.String("template-build-id", "", "template build in object storage")
	rehydrateBuildID := fs.String("rehydrate-build-id", "",
		"a previous lifetime's writes to replay into the cache before serving")
	cacheRoot := fs.String("cache-root", "", "local cache for remote builds")
	readyFD := fs.Int("ready-fd", 0, "fd to signal once the device is online")
	readOnly := fs.Bool("read-only", false, "refuse writes")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *device == "" || *index < 0 || *cachePath == "" || *controlSock == "" {
		return fmt.Errorf("nbd-handler: --device, --index, --cache and --control are required")
	}

	cfg := nbd.Config{
		Device: *device, Index: *index,
		CachePath: *cachePath, ControlSock: *controlSock,
		TemplateDir: *templateDir, CacheRoot: *cacheRoot,
		ReadyFD: *readyFD, ReadOnly: *readOnly,
	}

	var err error
	if cfg.TemplateBuildID, err = parseOptionalUUID(*templateBuildID); err != nil {
		return fmt.Errorf("nbd-handler: --template-build-id: %w", err)
	}
	if cfg.RehydrateBuildID, err = parseOptionalUUID(*rehydrateBuildID); err != nil {
		return fmt.Errorf("nbd-handler: --rehydrate-build-id: %w", err)
	}

	// SIGTERM cancels, which disconnects the device and unblocks NBD_DO_IT.
	// Exiting on the signal instead would leave the kernel holding a device
	// whose server is gone.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var store block.ObjectStore
	if cfg.TemplateBuildID != uuid.Nil || cfg.RehydrateBuildID != uuid.Nil {
		if store, err = newBlockStore(ctx); err != nil {
			return err
		}
	}

	slog.Info("nbd handler starting", "device", cfg.Device, "cache", cfg.CachePath)
	return nbd.Run(ctx, cfg, store)
}

// newBlockStore builds the object-storage client the handler reads builds
// through. Credentials come from the environment hostd passed down.
func newBlockStore(ctx context.Context) (block.ObjectStore, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.S3Bucket == "" {
		return nil, fmt.Errorf("nbd-handler: a remote build was requested but " +
			"PILOT_S3_BUCKET is not set")
	}
	return s3.New(ctx, s3.Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		// The same prefix hostd writes builds under. A handler reading from a
		// different namespace than the daemon wrote to fails on a missing
		// header, which reads as a corrupt build rather than a misconfigured
		// prefix.
		Prefix:    chunkPrefix,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
}

func parseOptionalUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(s)
}

// dispatchSubcommand runs a hidden subcommand and reports whether it did.
func dispatchSubcommand() bool {
	if len(os.Args) < 2 {
		return false
	}
	var run func([]string) error
	switch os.Args[1] {
	case nbd.SubcommandName:
		run = runNBDHandler
	case uffd.SubcommandName:
		run = runUffdHandler
	default:
		return false
	}
	if err := run(os.Args[2:]); err != nil {
		slog.Error("handler exited", "handler", os.Args[1], "err", err)
		os.Exit(1)
	}
	return true
}
