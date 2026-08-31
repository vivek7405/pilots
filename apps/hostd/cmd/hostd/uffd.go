package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// runUffdHandler is hostd re-executed as a memory-fault server for one machine.
//
// Separate process for the same reason as the disk handler, only more so: if
// this one dies the guest's next page fault never resolves and the VM hangs in
// an uninterruptible wait. Keeping it outside hostd is what makes restarting
// the daemon safe.
func runUffdHandler(args []string) error {
	fs := flag.NewFlagSet(uffd.SubcommandName, flag.ContinueOnError)

	socket := fs.String("socket", "", "unix socket Firecracker connects to")
	memFile := fs.String("memfile", "", "a plain memory image on local disk")
	buildID := fs.String("build-id", "", "chunked memory build in object storage")
	parentBuildID := fs.String("parent-build-id", "",
		"the template a diff build's unchanged ranges are served from")
	cacheRoot := fs.String("cache-root", "", "local cache for remote builds")
	prefetch := fs.String("prefetch", "",
		"fault order to replay, then rewrite with this run's order")
	readyFD := fs.Int("ready-fd", 0, "fd to signal once the socket is listening")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		return fmt.Errorf("uffd-handler: --socket is required")
	}

	cfg := uffd.Config{
		Socket: *socket, MemFile: *memFile, CacheRoot: *cacheRoot,
		PrefetchFile: *prefetch, ReadyFD: *readyFD,
	}

	var err error
	if cfg.BuildID, err = parseOptionalUUID(*buildID); err != nil {
		return fmt.Errorf("uffd-handler: --build-id: %w", err)
	}
	if cfg.ParentBuildID, err = parseOptionalUUID(*parentBuildID); err != nil {
		return fmt.Errorf("uffd-handler: --parent-build-id: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var store block.ObjectStore
	if cfg.BuildID != uuid.Nil || cfg.ParentBuildID != uuid.Nil {
		if store, err = newBlockStore(ctx); err != nil {
			return err
		}
	}

	slog.Info("uffd handler starting", "socket", cfg.Socket)
	return uffd.Run(ctx, cfg, store)
}
