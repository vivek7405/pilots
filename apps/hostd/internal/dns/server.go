// Package dns answers <machine>.internal inside every machine's network
// namespace.
//
// The server shape here -- the internal-domain suffix gate, the optional rr.
// and nearest. mode prefixes, forwarding everything else upstream -- is ported
// from uncloud (Apache-2.0), internal/machine/dns. What is different is the
// address source and the lifetime of an answer: uncloud resolves to container
// IPs handed out by a cluster-wide allocator, while a machine address here is
// derived from its owner's key and its slot, and moves the moment the machine
// is rescued onto another host.
//
// That difference is why every answer carries a near-zero TTL. A guest holding
// a 300-second answer for a machine that has been rescued is talking to
// nothing, and it will keep doing so long after the platform has finished
// recovering.
//
// What a short TTL does NOT do is save a connection that is already open. A
// pool holding a socket to a rescued machine's old address simply breaks --
// which is what every failover does, but it means anything talking over
// .internal needs reconnect logic of its own. "No human action" is a claim
// about the platform; it has never been a claim about an application's
// connections, and this is the place that distinction becomes visible.
package dns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

const (
	// InternalDomain is the suffix this server is authoritative for.
	InternalDomain = "internal."

	// Port is where the responder binds, on the address every guest already
	// calls its gateway.
	Port = 53

	// internalTTL is how long an answer may be cached, in seconds.
	//
	// One, not zero. The address a name resolves to changes whenever the
	// machine moves, so anything longer is a client talking confidently to an
	// address that belongs to someone else -- but a zero TTL is handled
	// inconsistently enough by real resolvers that one second is the safer way
	// to say the same thing.
	internalTTL = 1

	// maxConcurrentForwards bounds outstanding upstream queries across every
	// namespace on the host. A guest that floods DNS must not be able to open
	// unbounded sockets in the root namespace.
	maxConcurrentForwards = 1024

	forwardTimeout = 3 * time.Second
)

// Mode is which of a name's machines an answer should carry.
type Mode int

const (
	// ModeAll returns every healthy machine, shuffled, which is what a client
	// with no opinion gets.
	ModeAll Mode = iota
	// ModeRoundRobin is ModeAll asked for explicitly, via an rr. prefix.
	ModeRoundRobin
	// ModeNearest prefers machines on the querying machine's own host, which
	// is a loopback-speed hop rather than a mesh one.
	ModeNearest
)

// Query is one lookup, already parsed.
type Query struct {
	// MachineID is the machine that asked. Its app is what scopes the answer,
	// and it is known from the socket the query arrived on rather than from
	// anything in the packet -- a guest cannot claim to be in another app.
	MachineID string
	// Name is the bare machine name, with the mode prefix and the internal
	// suffix removed.
	Name string
	Mode Mode
}

// Resolver turns a query into addresses.
type Resolver interface {
	Resolve(q Query) []netip.Addr
}

// Server owns every namespace's responder.
//
// One handler and one upstream client for the whole host, with a socket per
// namespace hanging off them. A process per machine would be a thousand
// processes on a full host, for a service that is idle almost all of the time.
type Server struct {
	resolver  Resolver
	upstreams []netip.AddrPort
	client    *dns.Client
	forwards  chan struct{}

	mu    sync.Mutex
	bound map[string]*binding
}

// binding is one machine's pair of sockets.
type binding struct {
	udp *dns.Server
	tcp *dns.Server
}

// New returns a server that resolves through r and forwards everything else to
// upstreams.
func New(r Resolver, upstreams []netip.AddrPort) *Server {
	return &Server{
		resolver:  r,
		upstreams: upstreams,
		client:    &dns.Client{Timeout: forwardTimeout},
		forwards:  make(chan struct{}, maxConcurrentForwards),
		bound:     map[string]*binding{},
	}
}

// ParseUpstreams reads a comma-separated list of upstream servers, defaulting
// the port.
func ParseUpstreams(spec string) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if addr, err := netip.ParseAddr(field); err == nil {
			out = append(out, netip.AddrPortFrom(addr, Port))
			continue
		}
		ap, err := netip.ParseAddrPort(field)
		if err != nil {
			return nil, fmt.Errorf("dns: upstream %q is not an address: %w", field, err)
		}
		out = append(out, ap)
	}
	return out, nil
}

// Bind starts answering inside one machine's namespace.
//
// Idempotent: a machine that is already bound keeps the sockets it has. Wake
// and adopt both land here for machines that may or may not already be
// listening, and rebinding would drop queries in flight for no reason.
func (s *Server) Bind(machineID, netnsName string) error {
	s.mu.Lock()
	if _, ok := s.bound[machineID]; ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	addr := net.JoinHostPort(netns.TapHostIP, strconv.Itoa(Port))

	var (
		packet net.PacketConn
		stream net.Listener
	)
	// Both sockets are opened from inside the namespace, and nothing else is
	// done in there -- the fewer instructions run on a thread that is in
	// another namespace, the smaller the window in which a bug strands it.
	if err := netns.Do(netnsName, func() error {
		var err error
		if packet, err = net.ListenPacket("udp", addr); err != nil {
			return fmt.Errorf("udp: %w", err)
		}
		if stream, err = net.Listen("tcp", addr); err != nil {
			packet.Close()
			return fmt.Errorf("tcp: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("dns: bind in %s: %w", netnsName, err)
	}

	handler := s.handlerFor(machineID)
	b := &binding{
		udp: &dns.Server{PacketConn: packet, Handler: handler},
		tcp: &dns.Server{Listener: stream, Handler: handler},
	}

	s.mu.Lock()
	if _, ok := s.bound[machineID]; ok {
		// Another Bind won the race. Close ours rather than leaking the
		// sockets, and leave the winner serving.
		s.mu.Unlock()
		packet.Close()
		stream.Close()
		return nil
	}
	s.bound[machineID] = b
	s.mu.Unlock()

	go serve(b.udp, machineID, "udp")
	go serve(b.tcp, machineID, "tcp")
	return nil
}

func serve(srv *dns.Server, machineID, proto string) {
	if err := srv.ActivateAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("the .internal responder stopped",
			"machine", machineID, "proto", proto, "err", err)
	}
}

// Release closes a machine's sockets.
//
// Called when the namespace goes away. The sockets would die with it either
// way, but their goroutines would not: they would sit in ActivateAndServe on a
// namespace that no longer exists, once per machine, for the life of the
// process.
func (s *Server) Release(machineID string) {
	s.mu.Lock()
	b, ok := s.bound[machineID]
	delete(s.bound, machineID)
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = b.udp.Shutdown()
	_ = b.tcp.Shutdown()
}

// Close releases every binding.
func (s *Server) Close() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.bound))
	for id := range s.bound {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Release(id)
	}
}

// Bound reports how many namespaces are being served.
func (s *Server) Bound() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bound)
}

func (s *Server) handlerFor(machineID string) dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) == 0 {
			s.refuse(w, r)
			return
		}
		q := r.Question[0]
		if !dns.IsSubDomain(InternalDomain, q.Name) {
			s.forward(w, r)
			return
		}
		s.answerInternal(w, r, machineID)
	})
}

// answerInternal serves a name this host is authoritative for.
func (s *Server) answerInternal(w dns.ResponseWriter, r *dns.Msg, machineID string) {
	q := r.Question[0]
	name, mode := parseInternalName(q.Name)

	reply := new(dns.Msg)
	reply.SetReply(r)
	reply.Authoritative = true

	// A machine has no per-machine IPv4 address -- every guest in the fleet
	// shares 169.254.0.21 -- so an A query for a name that exists gets NOERROR
	// with no answer, never NXDOMAIN. A resolver doing A and AAAA together
	// treats NXDOMAIN on either as the name not existing, and the AAAA that
	// would have worked is discarded.
	if q.Qtype != dns.TypeAAAA && q.Qtype != dns.TypeANY {
		if len(s.resolver.Resolve(Query{MachineID: machineID, Name: name, Mode: mode})) == 0 {
			reply.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(reply)
		return
	}

	addrs := s.resolver.Resolve(Query{MachineID: machineID, Name: name, Mode: mode})
	if len(addrs) == 0 {
		reply.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(reply)
		return
	}
	for _, addr := range addrs {
		reply.Answer = append(reply.Answer, &dns.AAAA{
			Hdr: dns.RR_Header{
				Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET,
				Ttl: internalTTL,
			},
			AAAA: net.IP(addr.AsSlice()),
		})
	}
	_ = w.WriteMsg(reply)
}

// parseInternalName strips the mode prefix and the internal suffix.
//
// "rr." and "nearest." are uncloud's, and they are worth keeping: they let a
// client say what it wants from a name without the platform having to guess,
// and a client that says nothing gets the safe default.
func parseInternalName(qname string) (string, Mode) {
	// The suffix is trimmed before the trailing dot, so the bare apex
	// "internal." leaves an empty name rather than the literal "internal" --
	// which would otherwise be looked up as a machine called internal.
	name := strings.TrimSuffix(dns.CanonicalName(qname), InternalDomain)
	name = strings.TrimSuffix(name, ".")

	switch {
	case strings.HasPrefix(name, "rr."):
		return strings.TrimPrefix(name, "rr."), ModeRoundRobin
	case strings.HasPrefix(name, "nearest."):
		return strings.TrimPrefix(name, "nearest."), ModeNearest
	}
	return name, ModeAll
}

// forward sends a query the platform has no opinion about to an upstream.
//
// Forwarded from the ROOT namespace, not from the guest's. The alternative
// would open a socket per query inside the namespace, on a thread that has to
// be moved there and back; and the guest's own egress rules have nothing to
// say about a lookup, since it can already resolve anything it likes.
func (s *Server) forward(w dns.ResponseWriter, r *dns.Msg) {
	if len(s.upstreams) == 0 {
		s.refuse(w, r)
		return
	}

	select {
	case s.forwards <- struct{}{}:
		defer func() { <-s.forwards }()
	default:
		// Over the concurrency limit. SERVFAIL is honest and makes the client
		// retry, where queueing would let one noisy guest hold sockets open on
		// behalf of every other.
		s.fail(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()

	// The client's own transport, preserved: a query that arrived over TCP was
	// almost certainly a retry of one that did not fit in a UDP answer.
	network := "udp"
	if strings.HasPrefix(w.RemoteAddr().Network(), "tcp") {
		network = "tcp"
	}

	var lastErr error
	for _, upstream := range s.upstreams {
		client := *s.client
		client.Net = network
		resp, _, err := client.ExchangeContext(ctx, r, upstream.String())
		if err != nil {
			lastErr = err
			continue
		}
		// A truncated UDP answer is retried over TCP rather than passed on:
		// the guest asked over UDP and cannot know to retry itself.
		if resp.Truncated && network == "udp" {
			tcp := *s.client
			tcp.Net = "tcp"
			if full, _, terr := tcp.ExchangeContext(ctx, r, upstream.String()); terr == nil {
				resp = full
			}
		}
		_ = w.WriteMsg(resp)
		return
	}

	slog.Debug("no upstream answered", "err", lastErr)
	s.fail(w, r)
}

func (s *Server) refuse(w dns.ResponseWriter, r *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetRcode(r, dns.RcodeRefused)
	_ = w.WriteMsg(reply)
}

func (s *Server) fail(w dns.ResponseWriter, r *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(reply)
}
