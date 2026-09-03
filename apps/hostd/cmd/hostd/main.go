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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/build"
	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/dns"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/github"
	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/router"
	"github.com/vivek7405/pilots/hostd/internal/s3"
	"github.com/vivek7405/pilots/hostd/internal/seal"
	"github.com/vivek7405/pilots/hostd/internal/selfheal"
	"github.com/vivek7405/pilots/hostd/internal/services"
	"github.com/vivek7405/pilots/hostd/internal/volumes"
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

	// hostd re-executes itself to serve a machine's disk. Checked before
	// anything else, because a handler must not load daemon config, open the
	// state database, or bind a port.
	if dispatchSubcommand() {
		return
	}

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

	// Probe once, at startup, before anything can be created. The engine's
	// image copies use --reflink=auto, which falls back to a full copy without
	// reporting anything, so a host on the wrong filesystem is slow in a way
	// that looks like nothing is wrong. Say it out loud instead.
	reflink := fc.SupportsReflink(cfg.ChrootBase)
	if !reflink {
		slog.Warn("this host's machine store cannot share extents, so every "+
			"image copy is a real copy: create and checkpoint will be several "+
			"times slower than the engine is designed for. Put "+
			"PILOT_CHROOT_BASE on btrfs, or on XFS formatted with reflink=1.",
			"chroot_base", cfg.ChrootBase)
	}

	// The daemon's own lifetime. Everything the fleet runs in the background
	// -- gossip subscriptions, mesh reconciliation, the self-heal loops --
	// hangs off this, so a shutdown stops them without stopping the machines.
	ctx, stopFleet := context.WithCancel(context.Background())
	defer stopFleet()

	store, cache, err := openState(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	if cfg.Fleet() {
		slog.Info("joining the fleet", "state", "corrosion", "agent", cfg.CorrosionAddr)
	}

	uploader, err := newUploader(cfg)
	if err != nil {
		return err
	}

	// One device pool per host: the kernel's nbd devices are a host resource,
	// and reconcile has to reserve the ones adopted machines still hold before
	// the manager can hand any out.
	devices := nbd.NewDevicePool(nbd.DefaultMaxDevices)

	// The host's mesh identity, loaded here rather than inside startMesh
	// because every netns slot's address is derived from it -- the manager
	// below cannot hand out a slot without it.
	//
	// Loaded even on a single box, where nothing peers: the derivation costs
	// one file read, and without it same-host guest-to-guest traffic and
	// .internal would work in a fleet and silently not on one machine, which
	// is the worst place for a behavioural difference to hide.
	var machinePrefix netip.Prefix
	meshKeys, err := mesh.LoadOrCreateKeys(cfg.MeshKeyPath())
	if err != nil {
		if cfg.Fleet() {
			return err
		}
		// A single box can still run machines; they just cannot reach each
		// other. Said out loud, because the symptom is a name that does not
		// resolve rather than an error anywhere.
		//
		// The prefix stays ZERO here rather than being derived from the
		// zero-valued key, which would give every host in this state the same
		// block and put their machines on each other's addresses.
		slog.Warn("no mesh identity, so machines on this host cannot reach "+
			"each other and .internal will not resolve", "err", err)
	} else {
		machinePrefix = meshKeys.MachinePrefix()
	}

	// A second client over the same bucket under the chunk prefix. Content-
	// addressed builds are named by uuid alone, so they need their own
	// namespace or a build id could collide with a machine's key.
	chunks, err := newChunkStore(cfg)
	if err != nil {
		return err
	}

	// Volumes need the bucket: their data and their metadata replica both live
	// in it. A host without one has no volumes at all, and says so on the
	// first request rather than failing partway through a create.
	var volumeManager machines.VolumeManager
	if cfg.S3Bucket != "" {
		volumeManager = volumes.New(volumes.Config{
			HostID:    cfg.HostID,
			Endpoint:  cfg.S3Endpoint,
			Region:    cfg.S3Region,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		})
	}

	// The fleet's view of itself, and the rule that turns a row into an
	// address. The tenant filter and the DNS responder read the SAME view, or
	// a machine could be resolvable and unreachable at the same instant.
	var view fleetView = storeView{store: store}
	if cache != nil {
		view = cache
	}
	locator := mesh.NewLocator(cfg.HostID, meshKeys.Public, view)

	// The .internal responder. Built here because the machine manager tells it
	// when a namespace appears.
	//
	// Built UNCONDITIONALLY, including on a host with no mesh identity. The
	// guest rootfs names the gateway as its ONLY nameserver, so this is not
	// merely where .internal is answered -- it is where every lookup a guest
	// makes is answered, and a host without it runs machines that cannot
	// resolve anything at all. Without a mesh identity the resolver simply
	// finds no machine addresses and everything is forwarded upstream, which
	// is the honest version of "nothing to discover".
	upstreams, err := dns.ParseUpstreams(cfg.DNSUpstream)
	if err != nil {
		return err
	}
	responder := dns.New(dns.NewFleetResolver(view, locator), upstreams)
	defer responder.Close()
	var discovery machines.Discovery = responder

	// The fleet key. Parsed at startup so a malformed one is a host that
	// refuses to start rather than a create that fails much later, after a
	// client has already handed over a secret.
	fleetKey, err := seal.ParseKey(cfg.FleetKey)
	if err != nil {
		return err
	}
	if !fleetKey.IsSet() {
		slog.Warn("no fleet key, so this host cannot store secrets: creates " +
			"carrying secret_env will be refused. Set PILOT_FLEET_KEY to the " +
			"same value on every host.")
	}

	mgr := machines.New(machines.Options{
		HostID:     cfg.HostID,
		Domain:     cfg.WorkloadDomain,
		StateRoot:  cfg.MachineStateRoot(),
		CacheRoot:  cfg.CacheRoot(),
		Store:      store,
		Uploader:   uploader,
		Chunks:     chunks,
		BlockStore: chunkReader(chunks),
		NBDDevices: devices,
		// The handlers are separate processes and read builds themselves, so
		// they need this daemon's storage credentials.
		HandlerEnv: os.Environ(),
		// Fleet-wide, so a host that rescues a machine can still reach it.
		AgentTokenSecret: cfg.AgentTokenSecret,
		Volumes:          volumeManager,
		MachinePrefix:    machinePrefix,
		Discovery:        discovery,
		FleetKey:         fleetKey,
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

	// The builder probes the local toolchain once at startup -- mke2fs here
	// may or may not read a tarball -- so it is constructed before anything
	// can post a build rather than on the first request.
	var builder api.BuildRunner
	if cfg.S3Bucket != "" {
		builder = build.New(ctx, build.Options{
			WorkRoot:       filepath.Join(cfg.CacheRoot(), "builds-work"),
			BuildDir:       filepath.Join(cfg.CacheRoot(), "builds"),
			Chunks:         chunks,
			AgentBinary:    cfg.GuestAgentBin,
			BuildkitSock:   cfg.BuildkitSock,
			CacheBucket:    cfg.S3Bucket,
			CacheEndpoint:  cfg.S3Endpoint,
			CacheRegion:    cfg.S3Region,
			CacheAccessKey: cfg.S3AccessKey,
			CacheSecretKey: cfg.S3SecretKey,
		})
	} else {
		slog.Warn("no object storage configured; builds are unavailable on this host")
	}

	// Re-adopt machines that outlived the previous hostd. This is what makes a
	// restart safe: the processes are still serving, and picking them back up
	// costs nothing.
	adopted := reconcile(cfg, mgr, devices)
	if adopted > 0 {
		slog.Info("re-adopted machines from a previous run", "count", adopted)
	}

	// After adoption, anything still lying around belongs to nothing.
	if err := mgr.GCOrphanInterfaces(); err != nil {
		slog.Warn("could not clean up orphaned interfaces", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go mgr.RunIdleMonitor(ctx)
	// Sweeps up Firecrackers this host has no record of -- the residue of a
	// hostd killed mid-create, or a destroy that failed partway.
	go mgr.RunReaper(ctx)

	// The fleet pieces, in the order they depend on each other: the mesh
	// carries gossip and forwarded requests, so it comes up before anything
	// that rides it.
	var f *fleet
	if cfg.Fleet() {
		f = &fleet{store: store, cache: cache}
		if cfg.MeshEnabled {
			dev, err := startMesh(ctx, cfg, cache, meshKeys)
			if err != nil {
				return fmt.Errorf("bring up the mesh: %w", err)
			}
			defer dev.Close()
			f.dev, f.keys = dev, meshKeys
		}
	}

	if machinePrefix.IsValid() {
		go runTenantFilter(ctx, cfg.HostID, view, locator)
	}

	routerOpts := router.Options{
		Domain:  cfg.WorkloadDomain,
		HostID:  cfg.HostID,
		Store:   store,
		Manager: mgr,
		SlotFor: mgr.SlotFor,
	}
	if f != nil {
		routerOpts.Peers = peers{cache: f.cache}
		// A host that finds the owner gone rescues the machine HERE, holding
		// the client, rather than failing until the rescue loop's next tick.
		routerOpts.Rescue = mgr.Rescue
		// ...but only the host the hash names, so two survivors cannot both
		// claim and both start a Firecracker on one machine's state. Same rule
		// and same function the rescue loop uses; there is only one definition
		// of it on purpose.
		routerOpts.RescuerFor = func(machineID string) (string, bool) {
			return selfheal.RescuerFor(machineID,
				f.cache.LiveHosts(time.Now(), selfheal.DeadAfter))
		}
		// The hot path reads the subscription cache, not the agent.
		routerOpts.Lookup = f.cache.MachineByName
	}
	rtr := router.New(routerOpts)

	// One listener, two audiences: requests for a workload hostname are
	// proxied into a machine, everything else is the control API. Keeping them
	// on one port means a host needs exactly one address to be useful.
	// The rollout drives machines through the same manager the API does, so a
	// deploy's replicas are ordinary machines with ordinary lifecycles.
	rollout := services.New(services.Options{
		HostID: cfg.HostID, Store: store, Machines: mgr,
		Peers: peerCaller(f),
	})

	// Only the arbiter for a service acts on it, so every host can run this
	// loop: they all see every service in their local replica, and all but one
	// will decline for any given service.
	go rollout.RunAutoscaler(ctx, mgr)

	// Traffic to a suspended service replica's address brings it back. Runs
	// beside the tenant filter that writes the counters it reads.
	go runWaker(ctx, cfg.HostID, view, mgr)

	// Custom domains verify on a loop, not only at registration: a CNAME is
	// almost always set after the domain is registered.
	go runDomainVerifier(ctx, cfg.HostID, store, cfg.WorkloadDomain)

	// Push-to-deploy and pull-request previews. The webhook is an ordinary
	// route on every host; exactly one acts on any delivery.
	ghApp, err := github.LoadApp(cfg.GitHubAppID, cfg.GitHubKeyPath, cfg.GitHubWebhookKey)
	if err != nil {
		return err
	}

	// Org scoping and the revocation check run on every authenticated request,
	// so in a fleet they read the subscription cache -- a map lookup -- rather
	// than querying the corrosion agent per call.
	var tenancy api.TenancyView = api.StoreTenancy(store)
	if f != nil {
		tenancy = cachedTenancy{cache: f.cache, store: store}
	}

	controlAPI := api.Routes(api.Deps{
		HostID: cfg.HostID, Store: store, Machines: mgr, Reflink: reflink,
		Builds: builder, Rollout: rollout, Domain: cfg.WorkloadDomain,
		Peers: peerLookup(f), Tenancy: tenancy, BuildGate: &quota.HostGate{},
		GitHub: github.Handler(github.Deps{
			HostID: cfg.HostID, App: ghApp, Store: store, Builds: builder,
			Rollout: rollout, Machines: mgr, Domain: cfg.WorkloadDomain,
		}),
	})

	// Machine-scoped API calls go to the host that owns the machine. Without
	// this, "every host serves the full API" means every host answers and
	// most of them are wrong -- an exec against a machine running elsewhere
	// simply fails.
	owner := machineOwner(store)
	if f != nil {
		owner = cachedOwner(f.cache, owner)
	}
	// The forwarding marker is a fleet-internal signal set by peers proxying
	// over the mesh. Stripped here so a client on the public listener cannot
	// forge it and make a non-owner host act on a machine-scoped call.
	handler := router.StripForwardMarker(dispatch(cfg, rtr, rtr.ForwardAPI(owner, controlAPI)))

	if f != nil && f.dev != nil {
		// Peers reach the same dispatch, guarded so a forwarded request is
		// never forwarded again.
		internal := router.InternalAPIHandler(dispatch(cfg, rtr.InternalHandler(), controlAPI))
		if err := startInternalListener(ctx, f.dev, internal); err != nil {
			return err
		}
		startSelfHeal(ctx, cfg, f, mgr)
	}

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
	// TLS, when the fleet can share certificates. Serves the same handler on
	// :443 with on-demand issuance; the plain listener below stays for the
	// internal mesh and for fleets without object storage.
	if certClient, cerr := newCertStore(cfg); cerr == nil && certClient != nil {
		if err := startTLS(ctx, cfg, store, certClient, handler); err != nil {
			return err
		}
	} else if cerr != nil {
		slog.Warn("TLS is off: could not open the certificate store", "err", cerr)
	}

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
func reconcile(cfg *config.Config, mgr *machines.Manager, devices *nbd.DevicePool) int {
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
		m := fc.Adopted(r.State, cfg.MachineStateRoot(), devices)
		if err := mgr.Adopt(r.State.MachineID, m, r.State.SlotIdx); err != nil {
			slog.Error("could not adopt machine", "machine", r.State.MachineID, "err", err)
			continue
		}
		adopted++
	}
	return adopted
}

// chunkPrefix namespaces content-addressed builds inside the bucket.
const chunkPrefix = "chunks"

// newChunkStore builds the client that reads and writes builds.
func newChunkStore(cfg *config.Config) (fc.Uploader, error) {
	if cfg.S3Bucket == "" {
		return fc.UnconfiguredStore{}, nil
	}
	return s3.New(context.Background(), s3.Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		Prefix:    chunkPrefix,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
}

// chunkReader exposes a chunk store as the block layer's read surface.
//
// Nil rather than a stub when storage is unconfigured: a lazy read has no
// meaningful failure to return per page, so the restore must fail up front on
// a missing store instead of per fault, with the guest already resumed.
func chunkReader(up fc.Uploader) block.ObjectStore {
	if store, ok := up.(block.ObjectStore); ok {
		return store
	}
	return nil
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

// newCertStore opens the bucket certificates are shared through.
//
// The same bucket as everything else, under its own prefix: certificates have
// to be readable by every host for the same reason machine images do, and a
// second bucket would be a second thing to configure and a second thing to get
// wrong.
func newCertStore(cfg *config.Config) (*s3.Client, error) {
	if cfg.S3Bucket == "" {
		return nil, nil
	}
	return s3.New(context.Background(), s3.Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		Prefix: "certs", AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
}

// peerLookup resolves other hosts for service-write forwarding. Nil on a
// single box, where there is no one to forward to.
func peerLookup(f *fleet) api.PeerLookup {
	if f == nil {
		return nil
	}
	return peers{f.cache}
}

// sealerOrNil hands the API the seal key when this host has one.
//
// An untyped nil rather than a typed one: a typed nil satisfies the interface
// and would then be asked IsSet on a nil receiver, so the absence of a key has
// to be visible as a nil interface value.
func sealerOrNil(k seal.Key) api.Sealer {
	if !k.IsSet() {
		return nil
	}
	return k
}

// peerCaller lets the rollout act on machines other hosts hold.
func peerCaller(f *fleet) services.PeerCaller {
	if f == nil {
		return nil
	}
	return peerAPI{cache: f.cache, http: &http.Client{Timeout: 2 * time.Minute}}
}
