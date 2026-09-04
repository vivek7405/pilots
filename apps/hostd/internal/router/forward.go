package router

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
)

// The control API has the same any-host property the workload router does: a
// client may call exec, checkpoint or suspend against ANY host, and the one it
// reaches is usually not the one running the machine. Without forwarding, the
// receiving host tries to act on a machine it does not have and fails --
// which turns "every host serves the full API" into "every host answers, and
// two thirds of them are wrong".
//
// This wraps the API rather than living inside it, so the API handlers stay
// unaware of the fleet entirely.

// machinePath matches the machine-scoped routes: /v1/machines/<id>/<verb>.
//
// Only the scoped ones. Creating a machine, or listing them, is answerable
// anywhere -- creation places it here on purpose, and a list is a local read of
// replicated state.
var machinePath = regexp.MustCompile(`^/v1/machines/([^/]+)(/.*)?$`)

// spritePath is the name-keyed exec alias. It is machine-scoped like the
// routes above, but by NAME, so the owner lookup needs a resolution first.
var spritePath = regexp.MustCompile(`^/v1/sprites/([^/]+)/exec$`)

// machineIDShape matches an id minted by newID("m"). A path segment of this
// shape is tried as an id before it is scanned for as a name.
var machineIDShape = regexp.MustCompile(`^m-[0-9a-f]{24}$`)

// MachineOwner reports which host owns a machine.
type MachineOwner func(ctx context.Context, machineID string) (hostID string, ok bool)

// ForwardAPI proxies machine-scoped API calls to the host that owns the
// machine.
func (r *Router) ForwardAPI(owner MachineOwner, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.opts.Peers == nil || req.Header.Get(forwardedHeader) != "" {
			// Either a single box, or a request that has already been
			// forwarded once and must be served here or not at all.
			next.ServeHTTP(w, req)
			return
		}

		var id string
		if m := machinePath.FindStringSubmatch(req.URL.Path); m != nil {
			id = m[1]
		} else if alias := spritePath.FindStringSubmatch(req.URL.Path); alias != nil {
			// The alias carries a name. Resolving it here decides only WHERE
			// the call is served; the owning host resolves it again from its
			// own replica, so an empty answer is simply served locally and the
			// local handler answers its own 404.
			id = r.machineIDByName(req.Context(), owner, alias[1])
		}
		if id == "" {
			next.ServeHTTP(w, req)
			return
		}

		hostID, ok := owner(req.Context(), id)
		if !ok || hostID == "" || hostID == r.opts.HostID {
			next.ServeHTTP(w, req)
			return
		}
		if !r.opts.Peers.IsLive(hostID) {
			// The owner is gone. The local handler cannot serve another
			// host's machine on its own -- nothing below the API claims
			// ownership, so a bare local wake leaves the VM running unclaimed
			// while every row write after it fails ErrNotOwner. Rescue the
			// machine here first, exactly as the workload router does, then
			// let the local handler treat it as this host's own.
			if r.opts.Rescue == nil || r.opts.Store == nil {
				http.Error(w, `{"error":"machine unavailable: its host is not responding"}`,
					http.StatusServiceUnavailable)
				return
			}
			row, err := r.opts.Store.GetMachine(req.Context(), id)
			if err != nil {
				http.Error(w, `{"error":"machine unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			if row.HostID != r.opts.HostID {
				// Detached from the client for the same reason as the
				// router's rescue: the claim must not be stranded half-done
				// by a client that gives up mid-restore.
				if err := r.opts.Rescue(context.WithoutCancel(req.Context()), *row); err != nil {
					slog.Error("could not rescue a machine to serve an API call",
						"machine", id, "dead_host", hostID, "err", err)
					http.Error(w, `{"error":"machine unavailable"}`, http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, req)
			return
		}

		addr, ok := r.opts.Peers.InternalAddr(hostID)
		if !ok {
			next.ServeHTTP(w, req)
			return
		}

		target := &url.URL{Scheme: "http", Host: addr}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Director = func(out *http.Request) {
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
			out.Header.Set(forwardedHeader, r.opts.HostID)
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Error("could not forward an API call to the owning host",
				"machine", id, "owner", hostID, "err", err)
			http.Error(w, `{"error":"machine unavailable"}`, http.StatusBadGateway)
		}

		proxy.Transport = forwardTransport
		proxy.ServeHTTP(w, req)
	})
}

// StripForwardMarker removes the fleet-internal forwarding marker from
// requests arriving on the public listener.
//
// The marker is a transport fact -- only a peer proxying over the mesh sets
// it, and the internal listener is the only place it may be believed. A copy
// arriving from outside is forged: trusting it would let any client make a
// non-owner host act on a machine-scoped call locally, skipping both the
// forwarding and the liveness logic.
func StripForwardMarker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Del(forwardedHeader)
		next.ServeHTTP(w, req)
	})
}

// InternalAPIHandler serves API calls forwarded by peers.
//
// The one-hop rule again: a request that arrives here already marked has been
// forwarded once, and if this host does not own the machine either, forwarding
// again would bounce it between hosts whose views disagree.
func InternalAPIHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get(forwardedHeader) == "" {
			http.Error(w, `{"error":"internal listener requires a forwarding marker"}`,
				http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, req)
	})
}
