package router

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
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

// ForwardedHeader marks a request that has already been forwarded once.
//
// Exported because it is the fleet's ONE marker: the public listener strips
// it (StripForwardMarker), the internal listener requires it
// (InternalAPIHandler), the API's arbiter forwarding sets it, and so does a
// host calling a peer directly. A second name for the same transport fact is
// what made every peer call answer 400 at the internal listener.
const ForwardedHeader = "X-Pilot-Forwarded"

// forwardedHeader is the in-package spelling.
const forwardedHeader = ForwardedHeader

// forwardTimeout bounds how long a forwarded request may take to reach the
// owner and produce response HEADERS, including a wake on the far side.
// Generous: the owner may have to restore the machine first.
//
// Headers, not the whole exchange: exec streams, log follows and SSE are
// long-lived by design -- main.go sets no WriteTimeout for exactly that
// reason -- and a machine's behavior must not depend on whether the client's
// DNS pick happened to land on the owning host.
const forwardTimeout = HeldWakeWindow

// HeldWakeWindow is how long a request to a sleeping machine is HELD.
//
// One number for both paths, deliberately. A request to a machine on this host
// and a request to the same machine one host over must wait the same length of
// time, or a client can tell where a machine is by how long it waits -- which
// is the one thing the routing layer exists to hide.
//
// Long enough that a cold boot from object storage finishes inside it, and
// short enough that a wake which will never finish ends as an error somebody
// can act on rather than as a connection that hangs until a load balancer
// gives up and reports something less useful.
const HeldWakeWindow = 120 * time.Second

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
func (r *Router) forwardToOwner(w http.ResponseWriter, req *http.Request, t *Target) {
	r.forwardTo(w, req, t, t.Machine.HostID)
}

// forwardTo proxies a request to a named host.
//
// The target is not always the owner. When the owner is gone the request goes
// to whichever host is designated to rescue the machine, which is the only
// host allowed to claim it -- see serveOrForward.
func (r *Router) forwardTo(w http.ResponseWriter, req *http.Request, t *Target, hostID string) {
	m := t.Machine
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
	// The replay hook, the SAME one serveLocally installs, and it belongs here
	// for the same two reasons.
	//
	// Without it a cross-host request was the one case where Pilot-Replay did
	// nothing. The owning host's serveLocally sees no replay state -- the state
	// is a context value on the EDGE's request and does not travel over HTTP --
	// so it re-sets the header for the edge to act on, exactly as designed.
	// The edge's forwarding proxy then had no ModifyResponse, so the header
	// passed straight through: the application's routing instruction was
	// silently ignored, and the header, which names an internal machine,
	// reached the client. An app using fly-replay's idiom got a working replay
	// or a silent no-op depending on which host the machine happened to be on,
	// which is invariant 2 -- no request path may depend on a specific host --
	// broken in the one direction hardest to notice.
	proxy.ModifyResponse = captureReplay(req.Context(), t)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, errReplay) {
			// Not a failure: nothing has been written, and the edge is about
			// to send the request to the machine the response named.
			return
		}
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

		// A machine this host is HANDING OFF must not be woken here, whoever
		// asked.
		//
		// serveOrForward has always checked this; the internal listener did
		// not. Every host answers for every machine, so most requests reach
		// some other host first and arrive here forwarded -- and that path went
		// straight to serveLocally, waking a machine another host was in the
		// middle of claiming. The drain hold, bypassed by one hop.
		if r.opts.HandingOff != nil {
			if to, moving := r.opts.HandingOff(target.Machine.ID); moving && to != r.opts.HostID {
				if req.Header.Get(drainHopHeader) != "" {
					// Already one handoff hop deep. See drainHopHeader.
					http.Error(w, "machine is being moved", http.StatusServiceUnavailable)
					return
				}
				moved := *target
				moved.Machine.HostID = to
				req.Header.Set(drainHopHeader, r.opts.HostID)
				r.forwardToOwner(w, req, &moved)
				return
			}
		}

		// A machine being handed TO this host is held, not refused.
		//
		// The source forwards the request the moment it suspends the machine,
		// which is before the claim has committed here -- the row still names
		// the source, so the check below would answer "machine is not served
		// by this host". That is a 404 at the edge for a machine nobody has
		// lost, produced by a maintenance operation whose entire promise is
		// that a request arriving mid-move is HELD rather than failed.
		//
		// The marker only decides whether to WAIT; it grants nothing. The
		// claim is still authorised by the offer row, which names who offered
		// the machine, to whom, and whether the offer is the newest -- so a
		// forged marker buys an attacker a bounded wait and then the same
		// refusal.
		if target.Machine.HostID != r.opts.HostID && req.Header.Get(drainHopHeader) != "" {
			if fresh, ok := r.holdForClaim(req.Context(), req.Host); ok {
				target = fresh
			} else {
				// Still not ours. 503 rather than 404: the machine exists, is
				// not lost, and the next request will find it.
				http.Error(w, "machine is being moved", http.StatusServiceUnavailable)
				return
			}
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

// drainHopHeader marks the ONE extra forward a handoff is allowed.
//
// forwardedHeader bounds the graph at one edge, which is what stops two hosts
// with disagreeing views from forwarding to each other. A handoff needs one
// more: the edge forwards to the machine's owner, and by the time it lands
// that owner may have handed the machine on. That second hop is bounded by
// construction -- the marker is refused if it arrives twice -- so it cannot
// become the loop the first rule exists to prevent.
const drainHopHeader = "Pilot-Drain-Hop"

// holdForClaim waits until this host owns the machine a peer is handing it,
// re-resolving as it goes.
//
// A drain suspends the machine and forwards the in-flight request straight
// away, so the request arrives here BEFORE Take has committed the claim: the
// row still names the source. Refusing then turns a planned maintenance into a
// 404 at the edge. Waiting costs the client the tail of one restore, which is
// the cost the drain was designed around.
//
// Bounded, and the bound is the source's own: handoffTimeout in the machines
// package is how long it waits for one target before offering the machine
// elsewhere, so a hold past that is waiting on an offer that has been
// withdrawn.
func (r *Router) holdForClaim(ctx context.Context, host string) (*Target, bool) {
	deadline := time.Now().Add(handoffHold)
	for {
		fresh, err := r.resolve(ctx, host)
		if err == nil && fresh.Machine.HostID == r.opts.HostID {
			return fresh, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(handoffPoll):
		}
	}
}

// handoffHold is how long a request waits for a claim to land, and handoffPoll
// is how often it looks.
//
// The hold matches the machines package's handoffTimeout for one target. Past
// that the source has given up on this host too, so holding longer waits on
// nothing.
const (
	handoffHold = 60 * time.Second
	handoffPoll = 25 * time.Millisecond
)

// serveOrForward decides where a request goes.
//
// Three outcomes, and the third is the one that matters: if the owning host is
// gone, this host claims the machine and restores it HERE, holding the
// client's request throughout. That is what makes "kill the host a client is
// mid-request against" survivable -- the next request lands on a survivor and
// is served, rather than failing until some background loop notices.
func (r *Router) serveOrForward(w http.ResponseWriter, req *http.Request, target *Target) {
	m := target.Machine

	// A machine mid-DRAIN is a special case that has to come first: its row
	// still names this host, and its memory image has already been offered to
	// another one. Serving it here would wake a machine somebody else is in
	// the middle of claiming; refusing would make a planned operation
	// customer-visible, which is the whole thing a drain exists to avoid. So
	// the request follows the machine.
	if r.opts.HandingOff != nil {
		if to, moving := r.opts.HandingOff(m.ID); moving && to != r.opts.HostID {
			moved := *target
			moved.Machine.HostID = to
			// MARKED, so the target knows to wait for its own claim rather
			// than refusing a machine whose row still names this host. Without
			// the marker the target's internal listener answered "machine is
			// not served by this host" -- a 404 at the edge, out of a planned
			// operation, which is the one thing a drain exists to avoid.
			req.Header.Set(drainHopHeader, r.opts.HostID)
			r.forwardToOwner(w, req, &moved)
			return
		}
	}

	if m.HostID == "" || m.HostID == r.opts.HostID {
		r.serveLocally(w, req, target)
		return
	}
	if r.opts.Peers != nil && r.opts.Peers.IsLive(m.HostID) {
		r.forwardToOwner(w, req, target)
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
		r.forwardTo(w, req, target, rescuer)
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
	//
	// A machine with a hard limit queues here instead of piling on. Counted on
	// the OWNER host, which is where this runs: concurrency is a property of
	// the guest, and a fleet-wide count would need a round trip per request to
	// enforce a limit about one process.
	knobs := api.ParseKnobs(target.Machine.KindKnobs)
	if knobs.HardLimit > 0 {
		if !r.opts.Manager.BeginLimited(ctx, target.Machine.ID, knobs.HardLimit, hardLimitQueue) {
			metrics.RouterHardLimitRefusals.Inc()
			// Retry-After, because this is a queue that drained too slowly
			// rather than a machine that is broken. A client that backs off a
			// second usually finds the replica the autoscaler just started.
			w.Header().Set("Retry-After", "1")
			http.Error(w, "machine is at its hard_limit; retry", http.StatusServiceUnavailable)
			return
		}
	} else {
		r.opts.Manager.Begin(target.Machine.ID)
	}
	defer r.opts.Manager.End(target.Machine.ID)
	go r.opts.Manager.Touch(context.WithoutCancel(ctx), target.Machine.ID)

	r.proxyTo(w, req, slot, target.Port, target)
}
