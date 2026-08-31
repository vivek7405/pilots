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

// forwardTimeout bounds a cross-host request, including a wake on the far
// side. Generous: the owner may have to restore the machine first.
const forwardTimeout = 120 * time.Second

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
	addr, ok := r.opts.Peers.InternalAddr(m.HostID)
	if !ok {
		slog.Error("cannot forward: the owning host has no mesh address",
			"machine", m.ID, "owner", m.HostID)
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
		slog.Error("could not forward to the owning host",
			"machine", m.ID, "owner", m.HostID, "addr", target.Host, "err", err)
		http.Error(w, "machine unavailable", http.StatusBadGateway)
	}

	ctx, cancel := context.WithTimeout(req.Context(), forwardTimeout)
	defer cancel()
	proxy.ServeHTTP(w, req.WithContext(ctx))
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
			// The forwarding host's view is stale, or ownership moved while
			// the request was in flight. Refusing is what stops a loop; the
			// client's next request will be routed from a fresher view.
			slog.Warn("a peer forwarded a machine this host does not own",
				"machine", target.Machine.ID, "from", from, "owner", target.Machine.HostID)
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

	// The owner is not heartbeating. Rescue it here rather than making the
	// client wait for the self-heal loop's next tick.
	if r.opts.Rescue == nil {
		http.Error(w, "machine unavailable", http.StatusServiceUnavailable)
		return
	}
	slog.Info("serving a machine whose host is gone by rescuing it here",
		"machine", m.ID, "dead_host", m.HostID)

	if err := r.opts.Rescue(req.Context(), m); err != nil {
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
