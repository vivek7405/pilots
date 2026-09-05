package main

import (
	"reflect"
	"testing"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"

	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/s3"
)

// A fleet with no Cloudflare token must advertise NO DNS-01 solver.
//
// This is the case that cannot be caught by reading the code: returning
// (*certmagic.DNS01Solver)(nil) from a function typed to return the concrete
// pointer produces an interface value that is NOT nil, so certmagic would
// believe the issuer can solve DNS-01 and hand it a challenge with no
// provider behind it. The failure surfaces as a nil dereference during an
// order, minutes after startup, on whichever host happened to win the lock.
func TestNoCloudflareTokenMeansNoDNS01Solver(t *testing.T) {
	solver := dnsSolver(&config.Config{})
	if solver != nil {
		t.Fatalf("dnsSolver returned %#v with no token; certmagic reads any "+
			"non-nil value here as \"this issuer does DNS-01\"", solver)
	}
	// Belt and braces: reflect sees through the interface to a typed nil,
	// which the comparison above would miss if the signature ever changed.
	if v := reflect.ValueOf(solver); v.IsValid() && v.Kind() == reflect.Ptr && v.IsNil() {
		t.Fatal("dnsSolver returned a typed nil pointer, not a nil interface")
	}
}

// With a token, the solver is a DNS-01 solver driving the Cloudflare provider
// with that token. Nothing here touches the network.
func TestCloudflareTokenBuildsADNS01Solver(t *testing.T) {
	solver := dnsSolver(&config.Config{CloudflareAPIToken: "cf-token"})
	dns, ok := solver.(*certmagic.DNS01Solver)
	if !ok {
		t.Fatalf("dnsSolver returned %T, want *certmagic.DNS01Solver", solver)
	}
	provider, ok := dns.DNSManager.DNSProvider.(*cloudflare.Provider)
	if !ok {
		t.Fatalf("the solver drives %T, want *cloudflare.Provider", dns.DNSManager.DNSProvider)
	}
	if provider.APIToken != "cf-token" {
		t.Fatalf("the provider carries token %q, want the configured one", provider.APIToken)
	}
}

// The managed set is the wildcard plus BOTH apexes.
//
// The wildcard is the whole point -- it is what covers every machine URL and
// the API hostname, neither of which can be enumerated in advance. The
// workload apex is separate because a wildcard does not cover the name it
// wildcards, and the dashboard apex is a different zone on purpose.
func TestWildcardNamesCoverTheWildcardAndBothApexes(t *testing.T) {
	got := wildcardNames(&config.Config{
		WorkloadDomain:  "pilotrun.app",
		DashboardDomain: "pilots.run",
	})
	want := []string{"*.pilotrun.app", "pilotrun.app", "pilots.run"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managed names %v, want %v", got, want)
	}
}

// A fleet that has not configured a dashboard apex, or has pointed it at the
// workload apex, must not ask ACME for the same name twice: certmagic would
// order it twice and the second order is pure rate-limit spend.
func TestWildcardNamesDoNotRepeatTheApex(t *testing.T) {
	for _, dashboard := range []string{"", "pilotrun.app"} {
		got := wildcardNames(&config.Config{
			WorkloadDomain:  "pilotrun.app",
			DashboardDomain: dashboard,
		})
		want := []string{"*.pilotrun.app", "pilotrun.app"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("dashboard %q: managed names %v, want %v", dashboard, got, want)
		}
	}
}

// TLS needs both a place to share certificates from and a contact to register
// with, and the URL every client is told is rendered from the same predicate.
// One definition, read twice, so the two can never disagree: a host serving
// plain HTTP that reports https:// hands out links that do not open.
func TestTLSEnabledNeedsAStoreAndAContact(t *testing.T) {
	store := &s3.Client{}

	cases := []struct {
		name    string
		objects *s3.Client
		email   string
		want    bool
	}{
		{"no store, no contact", nil, "", false},
		{"no store, a contact", nil, "ops@pilots.run", false},
		{"a store, no contact", store, "", false},
		{"both", store, "ops@pilots.run", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ACMEEmail: tc.email}
			if got := tlsEnabled(cfg, tc.objects); got != tc.want {
				t.Errorf("tlsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}
