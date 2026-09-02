package certs

import (
	"context"
	"errors"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

type fakeDomains struct {
	rows []state.Domain
	err  error
	hits int
}

func (f *fakeDomains) ListDomains(ctx context.Context) ([]state.Domain, error) {
	f.hits++
	return f.rows, f.err
}

// The decision function is the whole abuse defence.
//
// Wildcard DNS points every host at *.pilotrun.app, so anyone can aim a
// hostname at our IPs and open a TLS connection. A permissive answer here
// turns the fleet into a free certificate mint and burns the Let's Encrypt
// rate limit for every real customer on it.
func TestUnknownNamesAreRefused(t *testing.T) {
	d := NewDecider(&fakeDomains{rows: []state.Domain{
		{Hostname: "app.example.com", ServiceID: "svc-1", VerifiedAt: 1},
	}})

	if err := d.Allow(context.Background(), "app.example.com"); err != nil {
		t.Errorf("a registered, verified domain was refused: %v", err)
	}
	if err := d.Allow(context.Background(), "attacker.example.net"); err == nil {
		t.Error("an unregistered name was allowed to mint a certificate")
	}
}

// A row exists as soon as someone asks for a domain; the CNAME check is what
// proves they control it. Issuing before that spends the fleet's rate limit on
// a name its owner may never point here.
func TestUnverifiedDomainsGetNoCertificate(t *testing.T) {
	d := NewDecider(&fakeDomains{rows: []state.Domain{
		{Hostname: "pending.example.com", ServiceID: "svc-1", VerifiedAt: 0},
	}})
	if err := d.Allow(context.Background(), "pending.example.com"); err == nil {
		t.Error("an unverified domain was issued a certificate")
	}
}

// A failed read refuses rather than allows: one wrongly refused handshake
// costs a retry, one wrongly allowed costs an unbounded issuance.
func TestAFailedLookupRefuses(t *testing.T) {
	d := NewDecider(&fakeDomains{err: errors.New("corrosion is down")})
	if err := d.Allow(context.Background(), "app.example.com"); err == nil {
		t.Error("a name was allowed while the domain list could not be read")
	}
}

// Case and a trailing dot are both normal in SNI and must not decide policy.
func TestNamesAreNormalised(t *testing.T) {
	d := NewDecider(&fakeDomains{rows: []state.Domain{
		{Hostname: "app.example.com", ServiceID: "svc-1", VerifiedAt: 1},
	}})
	for _, name := range []string{"APP.example.com", "app.example.com.", "App.Example.Com."} {
		if err := d.Allow(context.Background(), name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// The handshake path must not become a query per connection.
func TestLookupsAreCached(t *testing.T) {
	src := &fakeDomains{rows: []state.Domain{
		{Hostname: "app.example.com", ServiceID: "svc-1", VerifiedAt: 1},
	}}
	d := NewDecider(src)
	for i := 0; i < 50; i++ {
		if err := d.Allow(context.Background(), "app.example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if src.hits > 2 {
		t.Errorf("%d reads for 50 handshakes: the TLS hot path is querying per connection", src.hits)
	}
}
