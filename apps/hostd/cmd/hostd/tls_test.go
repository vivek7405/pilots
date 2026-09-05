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
// with. tlsConfigured is that pair read from configuration alone; tlsEnabled
// adds this host's store having opened. One definition, read twice, so the
// listener and the URL can never disagree about the configuration.
func TestTLSEnabledNeedsAStoreAndAContact(t *testing.T) {
	store := &s3.Client{}

	cases := []struct {
		name       string
		objects    *s3.Client
		bucket     string
		email      string
		configured bool
		want       bool
	}{
		{"no bucket, no contact", nil, "", "", false, false},
		{"no bucket, a contact", nil, "", "ops@pilots.run", false, false},
		{"a bucket, no contact", store, "pilots", "", false, false},
		{"both, store open", store, "pilots", "ops@pilots.run", true, true},
		// The one case that separates the two predicates: the fleet is
		// configured for TLS, this host's store did not open. It must not
		// serve TLS, and -- see the test below -- it must still render the
		// fleet's URLs.
		{"both, store failed to open", nil, "pilots", "ops@pilots.run", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{S3Bucket: tc.bucket, ACMEEmail: tc.email}
			if got := tlsConfigured(cfg); got != tc.configured {
				t.Errorf("tlsConfigured = %v, want %v", got, tc.configured)
			}
			if got := tlsEnabled(cfg, tc.objects); got != tc.want {
				t.Errorf("tlsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every host holding the same configuration renders the same URL, whether or
// not its own certificate store opened.
//
// This is AGENTS.md invariant 4 -- URLs are permanent -- and it is a fleet
// property, not a per-host one: every host answers for every machine and
// service row, not only its own. If the scheme were read from the answering
// host's runtime state, a transient S3 failure on one host, or the window
// during a rolling credentials change, would have that host report
// http://<name>.<domain>:8080 for machines its peers report
// https://<name>.<domain> for. Same machine, two URLs, decided by which host
// took the call.
func TestPublicURLIsTheSameOnEveryHostWithThisConfig(t *testing.T) {
	cfg := &config.Config{
		S3Bucket:   "pilots",
		ACMEEmail:  "ops@pilots.run",
		ListenAddr: ":8080",
	}
	const want = "https://webapp.pilotrun.app"

	hosts := []struct {
		name    string
		objects *s3.Client
		serves  bool
	}{
		{"certificate store open", &s3.Client{}, true},
		// newCertStore returned an error -- S3 refused the connection at
		// startup, or the credentials were mid-rotation -- so certClient is
		// nil and startTLS never ran on this host.
		{"certificate store failed to open", nil, false},
	}
	for _, h := range hosts {
		t.Run(h.name, func(t *testing.T) {
			// The fixture is the host it claims to be: these two genuinely
			// differ in whether they serve TLS.
			if got := tlsEnabled(cfg, h.objects); got != h.serves {
				t.Fatalf("tlsEnabled = %v, want %v", got, h.serves)
			}
			if got := publicURLFor(cfg).Of("webapp.pilotrun.app"); got != want {
				t.Errorf("renders %q, want %q -- every host with this "+
					"configuration must agree on a machine's URL", got, want)
			}
		})
	}
}

// The single box: no bucket, no contact, a plain listener on :8080. Every host
// with that configuration agrees on the port too.
func TestPublicURLOnASingleBoxCarriesThePlainPort(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":8080"}
	const want = "http://webapp.pilots.localhost:8080"
	if got := publicURLFor(cfg).Of("webapp.pilots.localhost"); got != want {
		t.Errorf("renders %q, want %q", got, want)
	}
}
