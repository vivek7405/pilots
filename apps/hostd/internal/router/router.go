// Package router is hostd's data plane.
//
// Every host runs an identical copy and can serve any request. Routing reads
// only local state, so a request is served without consulting any other host
// -- that is what keeps the data plane independent of anything central.
//
// The router also owns the X-Forwarded-* headers: it sets them once, at the
// public entry, so what reaches a guest describes this edge and not what a
// client claimed.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
// hardLimitQueue is how long a request waits for room on a machine at its
// hard limit before it is refused.
//
// Ten seconds because that is comfortably longer than a replica takes to
// start: the autoscaler reacts to soft_limit, and a request that waits out one
// scale-up succeeds instead of failing. Longer would turn a limit into a
// timeout, which is the failure it exists to prevent.
const hardLimitQueue = 10 * time.Second

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

	// Service resolves a service address to its current release's replicas
	// from the in-memory replica, as Lookup does for a machine name.
	// Optional; nil, and a miss, fall back to Store.ListServices and
	// Store.ListMachines.
	Service func(label string) (state.Service, []state.Machine, bool)

	// URLAuthOf says who may reach an object's URL: "public" or "org".
	// Nothing recorded is public. Reads local state only (rule 2).
	URLAuthOf func(ctx context.Context, id string) string
	// OrgOf is the owning org of a machine or service, from the local
	// tenancy replica.
	OrgOf func(ctx context.Context, id string) (string, bool)
	// KeyOrg resolves a bearer API key to its org, or false: unknown,
	// revoked, malformed.
	KeyOrg func(ctx context.Context, key string) (string, bool)
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
	//
	// A failed read is NOT returned here. The service branch below answers
	// from the subscription cache, which needs no store at all, and a store
	// that is briefly unwell must not turn a live service address into a 404.
	// The error is kept and returned only if nothing else resolves.
	rows, err := r.opts.Store.ListMachines(ctx)
	for _, row := range rows {
		if row.Name != name && row.Domain != strings.ToLower(host) {
			continue
		}
		// A builder is not routable, and this is the only place that can say
		// so: the loop above matches on NAME as well as domain, so clearing
		// the row's domain would not be enough.
		//
		// It serves no application -- nothing in a builder listens on the app
		// port -- so a request to its address can only ever fail. What it
		// would do first is WAKE it, and an org can read its own builder's
		// name out of the machine list, so without this a tenant could hold a
		// quota-exempt 4 vCPU machine awake indefinitely by curling a URL,
		// and every hit would reset the activity clock the stale-builder
		// collector reads. Nobody asked for that machine; it must not be
		// reachable from outside the host that made it.
		if machines.IsBuilder(row.Name) {
			continue
		}
		// Ownership is NOT checked here. Any host can resolve any machine --
		// DNS points every workload name at every host -- and what differs is
		// only where the request is then served. See serveOrForward.
		return &Target{Machine: row, Port: port}, nil
	}

	// Then a service address. A machine name wins, which is why this runs
	// after the loop above: the two live in one namespace, and the .internal
	// resolver breaks the same tie the same way. Both allocators refuse a
	// name the other holds, so a collision here means a cross-host race, not
	// an ordinary create.
	if svc, replicas, ok := r.serviceByLabel(ctx, name); ok {
		m, ok := pickReplica(replicas, r.opts.HostID)
		if !ok {
			return nil, &noReplicaError{label: name, service: svc.ID}
		}
		return &Target{Machine: m, Port: port}, nil
	}

	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("router: no machine named %q", name)
}

// noReplicaError is a service whose address resolves but which has nothing to
// serve from: it has never been deployed, or its current release has no
// machine left. It is NOT an unknown host, and answering 404 for it would tell
// the caller their URL is wrong when the URL is right and permanent.
type noReplicaError struct {
	label   string
	service string
}

func (e *noReplicaError) Error() string {
	return fmt.Sprintf("router: service %q (%s) has no machine on its current release",
		e.label, e.service)
}

// serviceByLabel resolves a service address to its current replicas, from the
// subscription cache when there is one and from a local store read otherwise.
//
// The store fallback repeats the cache's tie-break rather than sharing it: the
// cache holds a map and this holds a slice, and the rule is two lines. What
// matters is that both pick the lowest id, so a duplicated address routes the
// same way on a host whose subscription has the rows and one whose has not.
func (r *Router) serviceByLabel(ctx context.Context, label string) (state.Service, []state.Machine, bool) {
	// What the cache had, kept for the paths below that cannot better it. A
	// service the cache knows is a 503 rather than a 404 even when the store
	// read that follows fails, because its address is real either way.
	var (
		cached   state.Service
		cachedOK bool
	)
	if r.opts.Service != nil {
		if svc, replicas, ok := r.opts.Service(label); ok {
			if len(replicas) > 0 {
				return svc, replicas, true
			}
			// The address is in the cache and no machine of its current
			// release is. That is the honest answer for a service never
			// deployed, and a false 503 on a host whose machines subscription
			// is behind the release flip -- the two tables are delivered by
			// separate subscriptions and nothing orders them against each
			// other. So the store, which is read at the moment it is asked,
			// gets the last word.
			cached, cachedOK = svc, true
		}
	}
	if label == "" || r.opts.Store == nil {
		return cached, nil, cachedOK
	}

	services, err := r.opts.Store.ListServices(ctx)
	if err != nil {
		return cached, nil, cachedOK
	}
	var (
		found   state.Service
		matches int
	)
	for _, svc := range services {
		if svc.Domain != label {
			continue
		}
		matches++
		if matches == 1 || svc.ID < found.ID {
			found = svc
		}
	}
	if matches == 0 {
		return cached, nil, cachedOK
	}

	rows, err := r.opts.Store.ListMachines(ctx)
	if err != nil {
		return found, nil, true
	}
	return found, state.CurrentReplicas(found, rows), true
}

// pickReplica chooses which of a service's current replicas serves a request.
//
// A running replica on this host first, so a request that arrived here is
// served here rather than forwarded over the mesh. Then any running one. Then
// any at all, which is how a service whose replicas are suspended gets woken:
// the wake happens in ensureAwake, holding the request, exactly as it does for
// a machine reached by its own name.
//
// The choice among equals is random rather than first-match, so a service with
// several replicas spreads its load. The .internal resolver shuffles for the
// same reason.
func pickReplica(replicas []state.Machine, hostID string) (state.Machine, bool) {
	var local, running, any []state.Machine
	for _, m := range replicas {
		switch {
		case m.State == machines.StateRunning && m.HostID == hostID:
			local = append(local, m)
		case m.State == machines.StateRunning:
			running = append(running, m)
		default:
			any = append(any, m)
		}
	}
	for _, tier := range [][]state.Machine{local, running, any} {
		if len(tier) > 0 {
			return tier[rand.IntN(len(tier))], true
		}
	}
	return state.Machine{}, false
}

// machineIDByName resolves the alias's path segment for forwarding.
//
// The same order the API handler applies: an id-shaped value the owner lookup
// already knows wins, then the subscription cache, then a local list scan by
// name with the lowest id. Empty when nothing matches, which leaves the call to
// be served here and answered by the local handler's own 404.
func (r *Router) machineIDByName(ctx context.Context, owner MachineOwner, name string) string {
	if machineIDShape.MatchString(name) {
		if _, ok := owner(ctx, name); ok {
			return name
		}
	}
	if r.opts.Lookup != nil {
		if m, ok := r.opts.Lookup(name); ok {
			return m.ID
		}
	}
	if r.opts.Store == nil {
		return ""
	}
	rows, err := r.opts.Store.ListMachines(ctx)
	if err != nil {
		return ""
	}
	id := ""
	for _, row := range rows {
		if row.Name == name && row.State != state.StateDestroyed && (id == "" || row.ID < id) {
			id = row.ID
		}
	}
	return id
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

	setEdgeHeaders(req)

	target, err := r.resolve(ctx, req.Host)
	if err != nil {
		// A service that exists but has nothing to serve is not an unknown
		// host. Its address is permanent and correct; it has simply never
		// been deployed, so it is 503 and the body says which.
		var noReplica *noReplicaError
		if errors.As(err, &noReplica) {
			http.Error(w, "service has no release yet", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	if !r.allowed(w, req, target) {
		return
	}

	// The body is buffered before the first attempt, because a replay has to
	// send it again and a body can be read once. A request that carries none,
	// which is most of them, costs nothing here.
	body, bodyErr := bufferBody(req)
	tooLarge := bodyErr != nil
	if !tooLarge {
		rewind(req, body)
	}

	st := &replayState{}
	req = req.WithContext(withReplayState(req.Context(), st))
	r.serveOrForward(w, req, target)

	if st.want == "" || st.done {
		return
	}
	// The machine asked for this request to be served somewhere else. Nothing
	// of its own response reached the client: captureReplay closed it.
	st.done = true
	if tooLarge {
		http.Error(w, "request is too large to replay", http.StatusBadGateway)
		return
	}
	r.replay(w, req, st, body)
}

// replay sends a request a second time, to the machine the first one named.
func (r *Router) replay(w http.ResponseWriter, req *http.Request, st *replayState, body []byte) {
	want, err := parseReplay(st.want)
	if err != nil {
		// A routing instruction from a customer's process, so a spelling
		// nobody understands is refused rather than guessed at.
		slog.Warn("a machine asked for a replay this router could not read",
			"machine", st.answeredBy.Machine.ID, "err", err)
		http.Error(w, "replay refused: "+err.Error(), http.StatusBadGateway)
		return
	}

	next, err := r.replayTarget(want, st.answeredBy)
	if err != nil {
		slog.Warn("a machine asked for a replay that was refused",
			"machine", st.answeredBy.Machine.ID, "err", err)
		http.Error(w, "replay refused: "+err.Error(), http.StatusBadGateway)
		return
	}

	// What the second machine is told: which machine sent it here, on which
	// host, when, and whatever state the first one wanted carried across.
	req.Header.Set(ReplaySrcHeader, srcHeader(st.answeredBy, r.opts.HostID, want.State))
	rewind(req, body)
	r.serveOrForward(w, req, next)
}

// replayTarget resolves what a replay named, and refuses what it may not
// reach.
//
// The check is the whole security model, and it is made HERE rather than
// trusted from the header: the header was written by a customer's process, so
// treating it as authority over routing would let one tenant's application aim
// traffic at another tenant's machine.
func (r *Router) replayTarget(want replayRequest, from *Target) (*Target, error) {
	if want.Elsewhere {
		if from.Machine.ServiceID == "" {
			return nil, errors.New("elsewhere needs a service, and this machine is not part of one")
		}
		next, ok := r.otherReplica(from)
		if !ok {
			return nil, errors.New("no other replica of this service is available")
		}
		return next, nil
	}

	if r.opts.Lookup == nil {
		return nil, errors.New("this host cannot resolve a machine by name")
	}
	m, ok := r.opts.Lookup(want.Machine)
	if !ok {
		return nil, fmt.Errorf("no machine named %q", want.Machine)
	}
	// Same app, or same service. Both are the tenant's own namespace, and a
	// machine that shares neither is somebody else's by construction.
	sameApp := from.Machine.App != "" && m.App == from.Machine.App
	sameService := from.Machine.ServiceID != "" && m.ServiceID == from.Machine.ServiceID
	if !sameApp && !sameService {
		return nil, fmt.Errorf("%q is not in the same app or service", want.Machine)
	}
	return &Target{Machine: m, Port: from.Port}, nil
}

// otherReplica picks any running replica of this machine's service except this
// one.
func (r *Router) otherReplica(from *Target) (*Target, bool) {
	if r.opts.Service == nil {
		return nil, false
	}
	_, replicas, ok := r.opts.Service(from.Machine.ServiceID)
	if !ok {
		return nil, false
	}
	for _, m := range replicas {
		if m.ID != from.Machine.ID && m.State == "running" {
			return &Target{Machine: m, Port: from.Port}, true
		}
	}
	return nil, false
}

// setEdgeHeaders makes the forwarded headers say what THIS edge knows.
//
// The inbound X-Forwarded-For is deleted, not appended to: ReverseProxy adds
// the peer to whatever arrived, so a client that sent the header would sit
// leftmost, which is the entry a rate limiter behind us reads. Proto and Host
// are set here because TLS terminates here. Only the public entry runs this;
// a request forwarded over the mesh keeps the values the first hop set, and
// its own ReverseProxy appends the mesh peer after the client.
//
// The sibling client-IP headers go with it. Deleting X-Forwarded-For alone
// only moves the forgery: an application that reads X-Real-IP (the nginx
// convention, and what several frameworks fall back to), CF-Connecting-IP or
// RFC 7239 Forwarded would still be reading a value the caller chose. Nothing
// in front of this edge sets any of them, so any that arrives is a client's.
var clientIPHeaders = []string{
	"X-Forwarded-For",
	"X-Real-IP",
	"CF-Connecting-IP",
	"True-Client-IP",
	"Forwarded",
}

func setEdgeHeaders(req *http.Request) {
	for _, h := range clientIPHeaders {
		req.Header.Del(h)
	}
	proto := "http"
	if req.TLS != nil {
		proto = "https"
	}
	req.Header.Set("X-Forwarded-Proto", proto)
	req.Header.Set("X-Forwarded-Host", req.Host)
}

// agentAddr is the guest dial address, a variable so a test can point the
// proxy at a fake guest; GuestAgentPort is a constant and binding it in a test
// collides with a running host.
var agentAddr = (*netns.Slot).AgentAddr

// proxyTo forwards to the guest.
//
// Requests go to the guest agent, which forwards to the requested port inside
// the machine. That keeps one ingress path into the guest instead of exposing
// every application port on the host.
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, slot *netns.Slot,
	port int, machine *Target) {
	target := &url.URL{Scheme: "http", Host: agentAddr(slot)}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme = target.Scheme
		out.URL.Host = target.Host
		// The agent reads this and forwards inside the guest.
		out.Header.Set("X-Pilot-Proxy-Port", strconv.Itoa(port))
		// Host is preserved: applications build absolute URLs and set cookies
		// from it, so they must see what the user typed.
	}
	// The replay hook. It always strips the header, so a client never sees an
	// internal routing instruction, and aborts the response when the machine
	// asked for the request to be served somewhere else.
	proxy.ModifyResponse = captureReplay(req.Context(), machine)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, errReplay) {
			// Not a failure: nothing has been written, and ServeHTTP is about
			// to send the request to the machine the response named.
			return
		}
		slog.Error("proxy to guest failed", "addr", target.Host, "err", err)
		http.Error(w, "machine unreachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, req)
}

// allowed enforces url_auth on a resolved target. Public is the default and
// costs one local read. Org asks for an API key of the owning org in an
// Authorization: Bearer header: no key is 401 with a WWW-Authenticate so a
// client knows what to send, a key of another org is 403.
//
// BOTH the machine's own mode and its service's are read, and org on either
// gates. Reading only the service's would leave the mode unenforced for most
// machines: machines.provisionService mints a service row for EVERY machine
// created with an app or an environment, so `--url-auth org` on
// `pilot machines create api --env PORT=8080` would be written under the
// machine id, looked up under the service id, and silently serve the sandbox
// to anyone with the URL. Reading only the machine's would miss the replicas
// a service gained after promote, which never carried a mode of their own.
func (r *Router) allowed(w http.ResponseWriter, req *http.Request, t *Target) bool {
	if r.opts.URLAuthOf == nil {
		return true
	}
	ctx := req.Context()
	subject := t.Machine.ID
	if r.opts.URLAuthOf(ctx, subject) != urlAuthOrg {
		if t.Machine.ServiceID == "" || r.opts.URLAuthOf(ctx, t.Machine.ServiceID) != urlAuthOrg {
			return true
		}
		subject = t.Machine.ServiceID
	}
	key := ""
	if h := req.Header.Get("Authorization"); h != "" {
		if scheme, token, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "bearer") {
			key = token
		}
	}
	unauthorized := func() bool {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pilots"`)
		http.Error(w, "this URL is reachable with an API key of its org", http.StatusUnauthorized)
		return false
	}
	if key == "" || r.opts.KeyOrg == nil || r.opts.OrgOf == nil {
		return unauthorized()
	}
	org, ok := r.opts.KeyOrg(ctx, key)
	if !ok {
		return unauthorized()
	}
	owner, ok := r.opts.OrgOf(ctx, subject)
	if !ok || owner != org {
		http.Error(w, "this URL belongs to another org", http.StatusForbidden)
		return false
	}
	// The header was for the router, and the router has read it. Passing it on
	// would hand a fleet-wide API key of the org to whatever the machine runs
	// -- including a sandbox an agent is working in, which is the case this
	// mode exists for.
	req.Header.Del("Authorization")
	return true
}

// urlAuthOrg is the url_auth mode that gates a URL on an API key of the owning
// org. The string is the API's (api.URLAuthOrg), spelled again here because the
// router does not import that package.
const urlAuthOrg = "org"
