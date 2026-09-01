package router

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A machine lives on exactly one host, but a request for it can arrive at any
// of them: DNS points every workload name at every host's address. So a host
// that receives a request for someone else's machine forwards it over the
// mesh, and the client never learns that any of this happened -- its URL is
// permanent and its TLS terminated once, here.

// InternalPort is where a host listens for requests forwarded by its peers.
//
// Bound to the mesh address only. It carries no TLS and performs no
// authentication because it is not reachable from anywhere except inside the
// tunnel, which already authenticates and encrypts every byte.
const InternalPort = 51003

// InternalAddrOf is where a peer at a mesh address listens for forwarded
// requests. IPv6 literals must be bracketed or the port parses as part of the
// address.
func InternalAddrOf(meshAddr string) string {
	return net.JoinHostPort(meshAddr, strconv.Itoa(InternalPort))
}

// forwardedHeader marks a request that has already been forwarded once.
const forwardedHeader = "X-Pilot-Forwarded"

// forwardTimeout bounds how long a forwarded request may take to reach the
// owner and produce response HEADERS, including a wake on the far side.
// Generous: the owner may have to restore the machine first.
//
// Headers, not the whole exchange: exec streams, log follows and SSE are
// long-lived by design -- main.go sets no WriteTimeout for exactly that
// reason -- and a machine's behavior must not depend on whether the client's
// DNS pick happened to land on the owning host.
const forwardTimeout = 120 * time.Second

// forwardTransport carries forwarded requests over the mesh. Shared, so
// cross-host requests pool connections, and the place forwardTimeout is
// enforced without putting a deadline on the request itself.
var forwardTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	ResponseHeaderTimeout: forwardTimeout,
}

// Peers resolves other hosts on the mesh.
type Peers interface {
	// InternalAddr is the host:port to forward to, inside the tunnel.
	InternalAddr(hostID string) (string, bool)
	// IsLive reports whether a host is still heartbeating.
	IsLive(hostID string) bool
}

// forwardToOwner proxies a request to the host that owns the machine.
//
// Plaintext, over the mesh, to the owner's internal port -- NOT to its public
// :443. The mesh address has no certificate, so TLS there would mean forging
// SNI and paying a second handshake for transport security WireGuard already
// provides. TLS terminates once, at whichever host the client actually
// reached.
func (r *Router) forwardToOwner(w http.ResponseWriter, req *http.Request, m state.Machine) {
	r.forwardTo(w, req, m, m.HostID)
}

// forwardTo proxies a request to a named host.
//
// The target is not always the owner. When the owner is gone the request goes
// to whichever host is designated to rescue the machine, which is the only
// host allowed to claim it -- see serveOrForward.
func (r *Router) forwardTo(w http.ResponseWriter, req *http.Request, m state.Machine, hostID string) {
	addr, ok := r.opts.Peers.InternalAddr(hostID)
	if !ok {
		slog.Error("cannot forward: the target host has no mesh address",
			"machine", m.ID, "target", hostID)
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}

	target := &url.URL{Scheme: "http", Host: addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme = target.Scheme
		out.URL.Host = target.Host
		// One hop, and this is what enforces it. Two hosts with briefly
		// disagreeing cached rows -- which happens during a claim -- would
		// otherwise forward to each other until something timed out.
		out.Header.Set(forwardedHeader, r.opts.HostID)
		// Host is preserved end to end: the owner resolves the machine from
		// it, and the application behind it builds URLs and sets cookies from
		// what the user typed.
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("could not forward to the target host",
			"machine", m.ID, "target", hostID, "addr", target.Host, "err", err)
		http.Error(w, "machine unavailable", http.StatusBadGateway)
	}

	proxy.Transport = forwardTransport
	proxy.ServeHTTP(w, req)
}

// InternalHandler serves requests forwarded by peers.
//
// It is the same routing logic, with two differences: a request that has
// already been forwarded is refused rather than forwarded again, and a machine
// this host does not own is a 404 rather than another hop. Between them those
// make the forwarding graph exactly one edge deep.
func (r *Router) InternalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		from := req.Header.Get(forwardedHeader)
		if from == "" {
			// Nothing should reach this listener directly. It is bound to the
			// mesh, so this means a peer forwarded without marking it.
			http.Error(w, "internal listener requires a forwarding marker",
				http.StatusBadRequest)
			return
		}

		target, err := r.resolve(req.Context(), req.Host)
		if err != nil {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		if target.Machine.HostID != r.opts.HostID {
			// A peer forwards an orphan here when this host is the one
			// designated to rescue it. Accept exactly that case: the owner
			// must actually be gone, and the hash must actually name us.
			owner := target.Machine.HostID
			ownerGone := r.opts.Peers == nil || !r.opts.Peers.IsLive(owner)
			rescuer, ok := "", false
			if r.opts.RescuerFor != nil {
				rescuer, ok = r.opts.RescuerFor(target.Machine.ID)
			}
			if ownerGone && ok && rescuer == r.opts.HostID {
				r.rescueAndServe(w, req, target)
				return
			}

			// Otherwise the forwarding host's view is stale, or ownership
			// moved while the request was in flight. Refusing is what stops a
			// loop; the client's next request is routed from a fresher view.
			slog.Warn("a peer forwarded a machine this host does not own",
				"machine", target.Machine.ID, "from", from, "owner", owner)
			http.Error(w, "machine is not served by this host", http.StatusNotFound)
			return
		}
		r.serveLocally(w, req, target)
	})
}

// serveOrForward decides where a request goes.
//
// Three outcomes, and the third is the one that matters: if the owning host is
// gone, this host claims the machine and restores it HERE, holding the
// client's request throughout. That is what makes "kill the host a client is
// mid-request against" survivable -- the next request lands on a survivor and
// is served, rather than failing until some background loop notices.
func (r *Router) serveOrForward(w http.ResponseWriter, req *http.Request, target *Target) {
	m := target.Machine

	if m.HostID == "" || m.HostID == r.opts.HostID {
		r.serveLocally(w, req, target)
		return
	}
	if r.opts.Peers != nil && r.opts.Peers.IsLive(m.HostID) {
		r.forwardToOwner(w, req, m)
		return
	}

	// The owner is not heartbeating, so the machine has to be rescued before
	// this request can be served. Exactly one host may do that.
	//
	// Liveness here is read from a local CRDT replica, and so is the claim
	// that follows it. Neither can exclude anything: two survivors reading
	// their own replicas both see the owner as dead, both claim successfully,
	// and both start a Firecracker on one machine's state until last-write-
	// wins picks a loser. The fix is the rule the rescue loop already uses --
	// hash the machine id over the sorted live hosts -- because it needs no
	// coordination to give the same answer everywhere.
	r.rescueAndServe(w, req, target)
}

// rescueAndServe brings a machine back from a dead host and serves the request
// that asked for it, or hands the job to the host whose job it is.
func (r *Router) rescueAndServe(w http.ResponseWriter, req *http.Request, target *Target) {
	m := target.Machine

	if r.opts.Rescue == nil || r.opts.RescuerFor == nil {
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}
	rescuer, ok := r.opts.RescuerFor(m.ID)
	if !ok {
		// No live fleet to hash over -- including the case where this host's
		// own heartbeat is stale, which makes it the wrong one to be taking on
		// work. The rescue loop will pick this up once the view settles.
		slog.Warn("cannot decide who rescues a machine; leaving it",
			"machine", m.ID, "dead_host", m.HostID)
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}
	if rescuer != r.opts.HostID {
		// Someone else's to claim. Forward rather than refuse, so the client
		// is still served by this one request -- which is the whole point of
		// rescuing on the request path. Still one hop: the marker is set, and
		// the far side will not forward again.
		if req.Header.Get(forwardedHeader) != "" {
			http.Error(w, "machine is not served by this host", http.StatusNotFound)
			return
		}
		slog.Info("forwarding to the host designated to rescue a machine",
			"machine", m.ID, "dead_host", m.HostID, "rescuer", rescuer)
		r.forwardTo(w, req, m, rescuer)
		return
	}

	slog.Info("serving a machine whose host is gone by rescuing it here",
		"machine", m.ID, "dead_host", m.HostID)

	// Detached from the client: ClaimMachine commits early in the rescue, and
	// a client that gives up mid-restore must not strand the machine claimed
	// by this host but never restored -- the same reasoning as ensureAwake's
	// context.WithoutCancel.
	if err := r.opts.Rescue(context.WithoutCancel(req.Context()), m); err != nil {
		slog.Error("could not rescue a machine to serve a request",
			"machine", m.ID, "err", err)
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}

	// Re-read: the rescue moved the row, and the local path needs the machine
	// as it now stands.
	row, err := r.opts.Store.GetMachine(req.Context(), m.ID)
	if err != nil {
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}
	r.serveLocally(w, req, &Target{Machine: *row, Port: target.Port})
}

// serveLocally wakes and proxies to a machine on this host.
func (r *Router) serveLocally(w http.ResponseWriter, req *http.Request, target *Target) {
	ctx := req.Context()

	if err := r.ensureAwake(ctx, target.Machine); err != nil {
		slog.Error("could not wake machine for request",
			"machine", target.Machine.ID, "err", err)
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}

	slot, ok := r.opts.SlotFor(target.Machine.ID)
	if !ok {
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}

	// Counted while in flight so the idle monitor cannot suspend the machine
	// mid-response, and recorded so it is not suspended immediately after.
	r.opts.Manager.Begin(target.Machine.ID)
	defer r.opts.Manager.End(target.Machine.ID)
	go r.opts.Manager.Touch(context.WithoutCancel(ctx), target.Machine.ID)

	r.proxyTo(w, req, slot, target.Port)
}
