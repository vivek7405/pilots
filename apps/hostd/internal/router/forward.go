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

		m := machinePath.FindStringSubmatch(req.URL.Path)
		if m == nil {
			next.ServeHTTP(w, req)
			return
		}

		hostID, ok := owner(req.Context(), m[1])
		if !ok || hostID == "" || hostID == r.opts.HostID {
			next.ServeHTTP(w, req)
			return
		}
		if !r.opts.Peers.IsLive(hostID) {
			// The owner is gone. Serving it here means rescuing it here, which
			// the machine layer does under its own lock -- so hand it to the
			// local handler and let the rescue happen on the way.
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
				"machine", m[1], "owner", hostID, "err", err)
			http.Error(w, `{"error":"machine unavailable"}`, http.StatusBadGateway)
		}

		ctx, cancel := context.WithTimeout(req.Context(), forwardTimeout)
		defer cancel()
		proxy.ServeHTTP(w, req.WithContext(ctx))
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
