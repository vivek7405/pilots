package main

import (
	"bytes"
	"context"
	"encoding/json"
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
//
// The keys arrive from the caller because they are loaded before the machine
// manager: a slot's mesh address derives from them, so they cannot wait for
// the mesh to come up.
func startMesh(ctx context.Context, cfg *config.Config, cache *corrosion.Cache,
	keys mesh.Keys) (*mesh.Device, error) {

	dev, err := mesh.Open(keys)
	if err != nil {
		return nil, err
	}

	// The peer this host was told about, if any. The first host in a cluster
	// has none and needs none: everyone else comes to it.
	var bootstrap []mesh.Peer
	if cfg.MeshBootstrap != "" {
		peer, err := mesh.ParseBootstrapPeer(cfg.MeshBootstrap)
		if err != nil {
			dev.Close()
			return nil, err
		}
		bootstrap = append(bootstrap, peer)
		slog.Info("joining through a bootstrap peer",
			"addr", peer.Address, "endpoint", peer.Endpoint)
	}

	slog.Info("mesh up", "addr", dev.Address(), "pubkey", dev.PublicKey().String())
	go mesh.Reconcile(ctx, dev, cache, cfg.HostID, bootstrap)
	return dev, nil
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
func startInternalListener(ctx context.Context, dev *mesh.Device, internal http.Handler) error {
	addr := net.JoinHostPort(dev.Address().String(), strconv.Itoa(router.InternalPort))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on the mesh at %s: %w", addr, err)
	}

	// The internal listener serves BOTH audiences the public one does: a
	// workload hostname goes to the router, anything else is a forwarded API
	// call. Both refuse a request that arrives without the forwarding marker.
	srv := &http.Server{Handler: internal}
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
				MemFreeMiB: freeMemMiB(cfg.HugePages),
			}
		},
		Capacity: func(vcpus, memMiB int) bool {
			return memMiB <= freeMemMiB(cfg.HugePages)
		},
		Restore:        func(ctx context.Context, m *state.Machine) error { return mgr.Rescue(ctx, *m) },
		RunningLocally: mgr.RunningIDs,
		StopLocal:      mgr.StopLocal,
	}

	go selfheal.RunHeartbeat(ctx, opts)
	go selfheal.RunRescue(ctx, opts)
}

// freeMemMiB reports how much memory this host can still give to guests.
//
// MemAvailable, not MemFree: MemFree excludes reclaimable page cache, so a
// host with a warm cache would look full and refuse every rescue.
//
// And under 2MiB backing, neither one: reserved hugepages are subtracted from
// MemFree AND MemAvailable outright, because they are no longer available to
// anything but a hugepage mapping. A host reserving most of its RAM as a pool
// therefore reads as nearly full, advertises ~0 free to the whole fleet, and
// refuses every self-heal rescue -- with nothing in any log naming the cause.
// When the pool is what guests come out of, the pool is what to count.
func freeMemMiB(hugePages bool) int {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	if !hugePages {
		return meminfoKey(raw, "MemAvailable") / 1024
	}
	// HugePages_Free counts pages; Hugepagesize is in kB.
	return meminfoKey(raw, "HugePages_Free") * meminfoKey(raw, "Hugepagesize") / 1024
}

// meminfoKey pulls one numeric value out of /proc/meminfo, in whatever unit
// that key is published in. Missing reads as zero, which for every caller
// here means "no capacity", the safe direction.
func meminfoKey(raw []byte, key string) int {
	for line := range splitLines(string(raw)) {
		var n int
		if got, _ := fmt.Sscanf(line, key+": %d", &n); got == 1 {
			return n
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

// cachedOwner answers ownership from the subscription cache when it can -- a
// mutex and a map lookup instead of a query to the corrosion agent per
// machine-scoped API call -- falling back to the store for rows the
// subscription has not delivered yet.
func cachedOwner(cache *corrosion.Cache, fallback router.MachineOwner) router.MachineOwner {
	return func(ctx context.Context, machineID string) (string, bool) {
		if m, ok := cache.Machine(machineID); ok {
			return m.HostID, true
		}
		return fallback(ctx, machineID)
	}
}

// machineOwner resolves which host owns a machine, for API forwarding.
//
// A local read of replicated state, so it costs nothing and does not depend on
// the owner being reachable -- which matters, because the answer is often that
// the owner is gone.
func machineOwner(store state.Store) router.MachineOwner {
	return func(ctx context.Context, machineID string) (string, bool) {
		m, err := store.GetMachine(ctx, machineID)
		if err != nil {
			return "", false
		}
		return m.HostID, true
	}
}

// peerAPI calls another host's internal API.
//
// The internal listener serves the same routes as the public one, so a
// lifecycle call against a machine another host holds is the ordinary endpoint
// reached over the mesh rather than a second protocol.
type peerAPI struct {
	cache *corrosion.Cache
	http  *http.Client
	// token is the fleet peer credential. The far side serves the same
	// WithAuth-wrapped API to a peer as to a tenant, so a call without one is
	// a 401.
	token string
}

// peerURL resolves a host id to a URL on its internal listener.
func (p peerAPI) peerURL(hostID, path string) (string, error) {
	h, ok := p.cache.Host(hostID)
	if !ok || h.WGAddr == "" {
		return "", fmt.Errorf("hostd: %s has no mesh address", hostID)
	}
	return "http://" + router.InternalAddrOf(h.WGAddr) + path, nil
}

// mark sets what every peer call carries: the forwarding marker, so the far
// side does not forward it onward, so its internal listener accepts the
// request at all (InternalAPIHandler refuses one without it), and so its peer
// token is accepted; and the credential itself.
func (p peerAPI) mark(req *http.Request) {
	req.Header.Set(router.ForwardedHeader, "autoscaler")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
}

func (p peerAPI) Post(ctx context.Context, hostID, path string) error {
	url, err := p.peerURL(hostID, path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	p.mark(req)

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("hostd: %s %s: %s", hostID, path, resp.Status)
	}
	return nil
}

// PostJSON is Post with a body, for a call that names an image.
func (p peerAPI) PostJSON(ctx context.Context, hostID, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url, err := p.peerURL(hostID, path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	p.mark(req)

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("hostd: %s %s: %s", hostID, path, resp.Status)
	}
	return nil
}

// cachedTenancy answers org ownership and revocation from the subscription
// cache, in the shape of cachedOwner: a mutex and a map lookup instead of a
// query to the corrosion agent per authenticated request.
//
// A miss falls back to the store, exactly as cachedOwner does, because a miss
// is ambiguous: it is either "no row says who owns this" or "the subscription
// has not yet delivered the row this host wrote a moment ago". Reading the
// second as the first 404s a machine to the tenant that just created it --
// create, then exec, is the first thing an agent does, and the tenancy row is
// written milliseconds before that call arrives. The fallback costs a query
// only on the miss, so the steady state is still a map lookup.
type cachedTenancy struct {
	cache *corrosion.Cache
	store state.Store
}

func (t cachedTenancy) OrgOf(ctx context.Context, id string) (string, bool) {
	if org, ok := t.cache.OrgOf(id); ok {
		return org, true
	}
	row, err := t.store.GetTenancy(ctx, id)
	if err != nil {
		return "", false
	}
	return row.OrgID, true
}

func (t cachedTenancy) Revoked(_ context.Context, hash string) (bool, error) {
	return t.cache.Revoked(hash), nil
}

// storeVersion exposes the replica's version vector sum on /v1/health, or nil
// on SQLite where there is no replica and the field is 0.
//
// A type assertion rather than a method on state.Store: replication is a
// property of one backend, and putting it on the interface would make every
// implementation answer a question only one of them has.
func storeVersion(store state.Store) func(context.Context) (int64, error) {
	cs, ok := store.(*corrosion.Store)
	if !ok {
		return nil
	}
	return cs.Version
}

// cachedMachineCPU answers a machine's last start from the subscription cache.
//
// A map read, because it is on every machine read the API serves. There is no
// store fallback: an absent row means the machine has not started since this
// table existed, which is exactly what the empty answer says.
type cachedMachineCPU struct{ cache *corrosion.Cache }

func (v cachedMachineCPU) MachineCPU(_ context.Context, id string) (state.MachineCPU, bool) {
	return v.cache.MachineCPU(id)
}
