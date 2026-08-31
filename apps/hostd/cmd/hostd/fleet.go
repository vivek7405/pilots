package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/router"
	"github.com/vivek7405/pilots/hostd/internal/selfheal"
	"github.com/vivek7405/pilots/hostd/internal/state"
	"github.com/vivek7405/pilots/hostd/internal/state/corrosion"
)

// Everything that makes one host part of a fleet. On a single box none of it
// runs: the state store is local SQLite, there is no mesh, and every machine
// is this host's, so there is nothing to forward, rescue, or gossip.

// fleet is what a clustered host runs in addition to the single-box daemon.
type fleet struct {
	store state.Store
	cache *corrosion.Cache
	dev   *mesh.Device
	keys  mesh.Keys
}

// openState returns the state store this host should use.
//
// The interface is the same either way, which is the point: the router, the
// idle monitor and the machine manager cannot tell a replicated store from a
// local one, so nothing above this line has to care whether it is running in a
// fleet.
func openState(ctx context.Context, cfg *config.Config) (state.Store, *corrosion.Cache, error) {
	if !cfg.Fleet() {
		store, err := state.Open(cfg.StateDSN)
		return store, nil, err
	}

	client, err := corrosion.NewClient(cfg.CorrosionAddr, cfg.CorrosionToken)
	if err != nil {
		return nil, nil, err
	}

	// Wait for the agent to have APPLIED ITS SCHEMA, not merely to be
	// listening: corrosion serves its API first and would answer every read
	// with "no such table" until the schema lands.
	if err := corrosion.WaitReady(ctx, client, 2*time.Minute); err != nil {
		return nil, nil, err
	}

	cache, err := corrosion.NewCache(ctx, client)
	if err != nil {
		return nil, nil, fmt.Errorf("could not build the cluster cache: %w", err)
	}
	return corrosion.NewStore(client, cfg.HostID), cache, nil
}

// startMesh brings up WireGuard and keeps its peers matching the fleet.
func startMesh(ctx context.Context, cfg *config.Config, cache *corrosion.Cache) (*mesh.Device, mesh.Keys, error) {
	keys, err := mesh.LoadOrCreateKeys(cfg.MeshKeyPath())
	if err != nil {
		return nil, mesh.Keys{}, err
	}
	dev, err := mesh.Open(keys)
	if err != nil {
		return nil, keys, err
	}

	// The peer this host was told about, if any. The first host in a cluster
	// has none and needs none: everyone else comes to it.
	var bootstrap []mesh.Peer
	if cfg.MeshBootstrap != "" {
		peer, err := mesh.ParseBootstrapPeer(cfg.MeshBootstrap)
		if err != nil {
			dev.Close()
			return nil, keys, err
		}
		bootstrap = append(bootstrap, peer)
		slog.Info("joining through a bootstrap peer",
			"addr", peer.Address, "endpoint", peer.Endpoint)
	}

	slog.Info("mesh up", "addr", dev.Address(), "pubkey", dev.PublicKey().String())
	go mesh.Reconcile(ctx, dev, cache, cfg.HostID, bootstrap)
	return dev, keys, nil
}

// peers resolves other hosts for the router, from the same cache everything
// else reads.
type peers struct {
	cache *corrosion.Cache
}

func (p peers) InternalAddr(hostID string) (string, bool) {
	h, ok := p.cache.Host(hostID)
	if !ok || h.WGAddr == "" {
		return "", false
	}
	return router.InternalAddrOf(h.WGAddr), true
}

func (p peers) IsLive(hostID string) bool {
	return p.cache.IsLive(hostID, time.Now(), selfheal.DeadAfter)
}

// startInternalListener serves requests forwarded by peers.
//
// Bound to the mesh address ONLY. It carries no TLS and authenticates nothing,
// which is safe exactly because it is unreachable from outside the tunnel --
// and would be a hole the moment it were bound anywhere else.
func startInternalListener(ctx context.Context, dev *mesh.Device, r *router.Router) error {
	addr := net.JoinHostPort(dev.Address().String(), strconv.Itoa(router.InternalPort))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on the mesh at %s: %w", addr, err)
	}

	srv := &http.Server{Handler: r.InternalHandler()}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("the internal listener stopped", "err", err)
		}
	}()

	slog.Info("internal listener up", "addr", addr)
	return nil
}

// startSelfHeal runs the heartbeat and rescue loops.
func startSelfHeal(ctx context.Context, cfg *config.Config, f *fleet, mgr *machines.Manager) {
	opts := selfheal.Options{
		HostID: cfg.HostID,
		Fleet:  f.cache,
		Store:  f.store,
		Heartbeat: func() state.Host {
			return state.Host{
				ID:         cfg.HostID,
				WGAddr:     f.keys.Address().String(),
				WGPubKey:   f.keys.Public.String(),
				PublicIP:   cfg.PublicIP,
				CPUFree:    runtime.NumCPU(),
				MemFreeMiB: freeMemMiB(),
			}
		},
		Capacity: func(vcpus, memMiB int) bool {
			return memMiB <= freeMemMiB()
		},
		Restore:        func(ctx context.Context, m *state.Machine) error { return mgr.Rescue(ctx, *m) },
		RunningLocally: mgr.RunningIDs,
		StopLocal:      mgr.StopLocal,
	}

	go selfheal.RunHeartbeat(ctx, opts)
	go selfheal.RunRescue(ctx, opts)
}

// freeMemMiB reads available memory from the kernel.
//
// MemAvailable, not MemFree: MemFree excludes reclaimable page cache, so a
// host with a warm cache would look full and refuse every rescue.
func freeMemMiB() int {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range splitLines(string(raw)) {
		var kb int
		if n, _ := fmt.Sscanf(line, "MemAvailable: %d kB", &kb); n == 1 {
			return kb / 1024
		}
	}
	return 0
}

func splitLines(s string) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' {
				if !yield(s[start:i]) {
					return
				}
				start = i + 1
			}
		}
		if start < len(s) {
			yield(s[start:])
		}
	}
}
