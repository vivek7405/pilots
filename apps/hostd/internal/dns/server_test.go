package dns

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// capture is a ResponseWriter that keeps the reply instead of sending it.
type capture struct {
	msg   *dns.Msg
	proto string
}

func (c *capture) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("169.254.0.22"), Port: 53}
}
func (c *capture) RemoteAddr() net.Addr {
	if c.proto == "tcp" {
		return &net.TCPAddr{IP: net.ParseIP("169.254.0.21"), Port: 3000}
	}
	return &net.UDPAddr{IP: net.ParseIP("169.254.0.21"), Port: 3000}
}
func (c *capture) WriteMsg(m *dns.Msg) error { c.msg = m; return nil }
func (c *capture) Write([]byte) (int, error) { return 0, nil }
func (c *capture) Close() error              { return nil }
func (c *capture) TsigStatus() error         { return nil }
func (c *capture) TsigTimersOnly(bool)       {}
func (c *capture) Hijack()                   {}
func (c *capture) Network() string           { return c.proto }

// fixedResolver answers one name and nothing else.
type fixedResolver struct {
	name  string
	addrs []netip.Addr
}

func (f fixedResolver) Resolve(q Query) []netip.Addr {
	if q.Name != f.name {
		return nil
	}
	return f.addrs
}

func ask(t *testing.T, s *Server, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(qname), qtype)
	w := &capture{proto: "udp"}
	s.handlerFor("m-asker").ServeDNS(w, req)
	if w.msg == nil {
		t.Fatal("the handler wrote no reply")
	}
	return w.msg
}

func testServer() *Server {
	return New(fixedResolver{
		name:  "db",
		addrs: []netip.Addr{netip.MustParseAddr("fdcd:1::2")},
	}, nil)
}

// A rescued machine lands on a new host in a new slot, so its address changes.
// A guest holding a five-minute answer talks to nothing for five minutes after
// the platform has finished recovering.
func TestInternalAnswersAreEffectivelyUncacheable(t *testing.T) {
	reply := ask(t, testServer(), "db.internal", dns.TypeAAAA)

	if len(reply.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(reply.Answer))
	}
	aaaa, ok := reply.Answer[0].(*dns.AAAA)
	if !ok {
		t.Fatalf("answer is a %T, want AAAA", reply.Answer[0])
	}
	if aaaa.Hdr.Ttl > 1 {
		t.Errorf("TTL is %d; an answer that outlives a rescue points at another "+
			"machine entirely", aaaa.Hdr.Ttl)
	}
	if got := aaaa.AAAA.String(); got != "fdcd:1::2" {
		t.Errorf("answered %s", got)
	}
	if !reply.Authoritative {
		t.Error("the reply is not authoritative for a name this host owns")
	}
}

// Machines have no per-machine IPv4 address -- every guest in the fleet shares
// 169.254.0.21 -- so an A query for a name that DOES exist must come back
// empty and successful. NXDOMAIN there tells a resolver doing A and AAAA
// together that the name does not exist at all, and the AAAA that would have
// worked is thrown away.
func TestAQueriesForALiveNameAreEmptyRatherThanNXDOMAIN(t *testing.T) {
	reply := ask(t, testServer(), "db.internal", dns.TypeA)

	if reply.Rcode != dns.RcodeSuccess {
		t.Errorf("rcode is %s, want NOERROR", dns.RcodeToString[reply.Rcode])
	}
	if len(reply.Answer) != 0 {
		t.Errorf("an A query was answered with %v", reply.Answer)
	}
}

// A name in no app, or a machine that does not exist, is NXDOMAIN in both
// families -- there is nothing to find.
func TestAnUnknownNameIsNXDOMAIN(t *testing.T) {
	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeA} {
		reply := ask(t, testServer(), "nope.internal", qtype)
		if reply.Rcode != dns.RcodeNameError {
			t.Errorf("qtype %d: rcode is %s, want NXDOMAIN",
				qtype, dns.RcodeToString[reply.Rcode])
		}
	}
}

// Everything outside .internal is the internet's business. With no upstream
// configured it must fail loudly rather than answer NXDOMAIN, which would tell
// a guest that a real name does not exist.
func TestQueriesOutsideInternalAreNotAnsweredLocally(t *testing.T) {
	reply := ask(t, testServer(), "example.com", dns.TypeA)
	if reply.Rcode == dns.RcodeNameError {
		t.Error("an external name came back NXDOMAIN from the internal server")
	}
	if len(reply.Answer) != 0 {
		t.Errorf("an external name was answered locally: %v", reply.Answer)
	}
}

// The forward path is the one every non-.internal lookup in every guest takes,
// so it is worth exercising against a real upstream rather than trusting it.
func TestExternalQueriesAreForwardedUpstream(t *testing.T) {
	upstream, addr := startUpstream(t)
	defer upstream.Shutdown()

	s := New(fixedResolver{}, []netip.AddrPort{addr})
	reply := ask(t, s, "example.com", dns.TypeA)

	if len(reply.Answer) != 1 {
		t.Fatalf("forwarded query returned %d answers: %+v", len(reply.Answer), reply)
	}
	a, ok := reply.Answer[0].(*dns.A)
	if !ok || a.A.String() != "203.0.113.7" {
		t.Errorf("upstream answer did not come back intact: %v", reply.Answer[0])
	}
}

// startUpstream runs a stub resolver on loopback.
func startUpstream(t *testing.T) (*dns.Server, netip.AddrPort) {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(
		func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA,
					Class: dns.ClassINET, Ttl: 60},
				A: net.ParseIP("203.0.113.7"),
			})
			_ = w.WriteMsg(m)
		})}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub upstream never started")
	}
	return srv, netip.MustParseAddrPort(pc.LocalAddr().String())
}
