// Package router is hostd's data plane.
//
// Every host runs an identical copy and can serve any request. Routing reads
// only local state, so a request is served without consulting any other host
// -- that is what keeps the data plane independent of anything central.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Target names a machine and the port a request should reach inside it.
type Target struct {
	Machine state.Machine
	Port    int
}

// Options configures the router.
type Options struct {
	Domain string // e.g. "pilotrun.app"
	// HostID identifies this host. A machine owned by another host is not
	// this router's to start.
	HostID  string
	Store   state.Store
	Manager *machines.Manager
	// SlotFor resolves a running machine's network slot. The router needs the
	// slot's host-facing address to dial the guest.
	SlotFor func(machineID string) (*netns.Slot, bool)

	// Peers resolves other hosts on the mesh, for machines this host does not
	// own. Nil on a single-box deployment, where every machine is local.
	Peers Peers

	// RescuerFor names the one host allowed to rescue a machine, by the same
	// rule the self-heal loop uses. Without it a request-path rescue has no
	// way to exclude a second host doing the same thing at the same moment.
	// Nil disables rescuing on the request path entirely, which is correct for
	// a single-box deployment where there is nobody to rescue from.
	RescuerFor func(machineID string) (hostID string, ok bool)

	// Rescue claims a machine from a host that has stopped responding and
	// restores it here. Called on the request path, holding the client, so
	// that a host dying mid-request costs one slow request rather than an
	// outage lasting until a background loop notices.
	Rescue func(ctx context.Context, m state.Machine) error

	// Lookup resolves a machine by name from an in-memory replica, sparing
	// the routing hot path a store query per request -- which in a fleet is
	// an HTTP round trip to the corrosion agent. Optional; nil, and a miss
	// (a row the subscription has not delivered yet, or a custom domain),
	// fall back to Store.ListMachines.
	Lookup func(name string) (state.Machine, bool)
}

// Router proxies inbound requests to machines, waking them if needed.
type Router struct {
	opts  Options
	wakes sync.Map // machine id -> *wakeOnce
}

func New(opts Options) *Router { return &Router{opts: opts} }

// ParseHost splits a request hostname into a machine name and target port.
//
// Two shapes are supported:
//
//	<name>.<domain>          -> the machine's application port
//	<port>-<name>.<domain>   -> any port inside the machine
//
// The port-prefixed form is what lets a caller reach a service on an arbitrary
// port without the platform knowing anything about it in advance.
func ParseHost(host, domain string) (name string, port int, ok bool) {
	// Strip any port the client sent.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	suffix := "." + strings.ToLower(domain)
	if !strings.HasSuffix(host, suffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", 0, false
	}

	// A leading numeric segment is a port selector, but only when what follows
	// is a non-empty name -- otherwise "8080" alone would parse as a port with
	// no machine.
	if idx := strings.Index(label, "-"); idx > 0 {
		if p, err := strconv.Atoi(label[:idx]); err == nil && p > 0 && p <= 65535 {
			if rest := label[idx+1:]; rest != "" {
				return rest, p, true
			}
		}
	}
	return label, netns.GuestAppPort, true
}

// resolve finds the machine a hostname refers to.
func (r *Router) resolve(ctx context.Context, host string) (*Target, error) {
	name, port, ok := ParseHost(host, r.opts.Domain)
	if !ok {
		return nil, fmt.Errorf("router: %q is not a machine hostname", host)
	}

	// The subscription cache first: a mutex and a map lookup, no query at
	// all. A miss falls through to the store, so a machine the subscription
	// has not delivered yet -- or one reached by custom domain -- still
	// resolves.
	if r.opts.Lookup != nil {
		if m, ok := r.opts.Lookup(name); ok {
			return &Target{Machine: m, Port: port}, nil
		}
	}

	// A local read. This is the whole point: routing must not depend on any
	// other host being reachable.
	rows, err := r.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Name != name && row.Domain != strings.ToLower(host) {
			continue
		}
		// Ownership is NOT checked here. Any host can resolve any machine --
		// DNS points every workload name at every host -- and what differs is
		// only where the request is then served. See serveOrForward.
		return &Target{Machine: row, Port: port}, nil
	}
	return nil, fmt.Errorf("router: no machine named %q", name)
}

// wakeOnce coalesces concurrent wakes of one machine.
//
// A sleeping machine that suddenly receives fifty requests must be restored
// once, with the other forty-nine waiting on that same restore rather than
// each attempting their own.
type wakeOnce struct {
	once sync.Once
	err  error
	done chan struct{}
}

// ensureAwake restores a machine if it is not running, and holds the caller
// until it is serving.
//
// The request is HELD, never bounced to a holding page. A user should
// experience a scaled-to-zero machine as a slow response, not as an
// interstitial -- the moment scale-to-zero is visible it stops being a feature.
func (r *Router) ensureAwake(ctx context.Context, m state.Machine) error {
	if m.State == machines.StateRunning {
		return nil
	}

	knobs := machines.ParseKnobs(m.KindKnobs)
	if !knobs.AutoStart {
		return fmt.Errorf("router: machine %s is %s and does not auto-start", m.ID, m.State)
	}

	w := &wakeOnce{done: make(chan struct{})}
	actual, loaded := r.wakes.LoadOrStore(m.ID, w)
	w = actual.(*wakeOnce)

	if !loaded {
		go func() {
			w.once.Do(func() {
				start := time.Now()
				w.err = r.opts.Manager.Wake(context.WithoutCancel(ctx), m.ID)
				slog.Info("woke machine on request",
					"machine", m.ID, "took", time.Since(start), "err", w.err)
			})
			close(w.done)
			r.wakes.Delete(m.ID)
		}()
	}

	select {
	case <-w.done:
		return w.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ServeHTTP routes one request.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	target, err := r.resolve(ctx, req.Host)
	if err != nil {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	r.serveOrForward(w, req, target)
}

// proxyTo forwards to the guest.
//
// Requests go to the guest agent, which forwards to the requested port inside
// the machine. That keeps one ingress path into the guest instead of exposing
// every application port on the host.
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, slot *netns.Slot, port int) {
	target := &url.URL{Scheme: "http", Host: slot.AgentAddr()}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme = target.Scheme
		out.URL.Host = target.Host
		// The agent reads this and forwards inside the guest.
		out.Header.Set("X-Pilot-Proxy-Port", strconv.Itoa(port))
		// Host is preserved: applications build absolute URLs and set cookies
		// from it, so they must see what the user typed.
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("proxy to guest failed", "addr", target.Host, "err", err)
		http.Error(w, "machine unreachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, req)
}
