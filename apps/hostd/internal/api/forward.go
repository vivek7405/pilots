package api

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// PeerLookup resolves a host id to its internal listener address.
type PeerLookup interface {
	InternalAddr(hostID string) (string, bool)
}

// serviceArbiter names the host allowed to write a service row.
//
// The same rule the store enforces, computed here so a request can be sent to
// the right host rather than refused by the wrong one. Both sides read the
// same live set, so they agree -- and when they briefly do not, the store's
// refusal is the backstop.
func (d Deps) serviceArbiter(ctx context.Context, serviceID string) (string, bool) {
	hosts, err := d.Store.ListHosts(ctx)
	if err != nil {
		return "", false
	}
	live := make([]state.Host, 0, len(hosts))
	for _, h := range hosts {
		if time.Since(time.Unix(h.LastSeen, 0)) < 90*time.Second {
			live = append(live, h)
		}
	}
	return state.OwnerFor(serviceID, live)
}

// forwardToArbiter sends a service-scoped write to the host that owns it.
//
// Every host serves the full API, which for a service write means every host
// can RECEIVE one and only one may perform it. Refusing on the others would
// make a caller responsible for discovering the arbiter -- a rule that changes
// with fleet membership -- so the host that received it forwards instead.
//
// Returns false when this host is the arbiter and should handle it itself.
func (d Deps) forwardToArbiter(w http.ResponseWriter, r *http.Request, serviceID string) bool {
	// One hop. Two hosts with briefly disagreeing views of liveness would
	// otherwise forward to each other until something timed out.
	if r.Header.Get(forwardedHeader) != "" {
		return false
	}
	owner, ok := d.serviceArbiter(r.Context(), serviceID)
	if !ok || owner == d.HostID || d.Peers == nil {
		return false
	}
	addr, ok := d.Peers.InternalAddr(owner)
	if !ok {
		return false // no route to it; let the store refuse with a clear error
	}

	target := &url.URL{Scheme: "http", Host: addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
		out.Header.Set(forwardedHeader, d.HostID)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "the host that writes this service is unreachable: " + err.Error()})
	}
	proxy.ServeHTTP(w, r)
	return true
}

// forwardedHeader marks a request that has already been forwarded once.
const forwardedHeader = "X-Pilots-Forwarded-By"
