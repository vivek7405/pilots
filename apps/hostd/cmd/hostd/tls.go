package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/mholt/acmez/v3"

	"github.com/vivek7405/pilots/hostd/internal/certs"
	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/s3"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// startTLS serves the router over HTTPS, obtaining certificates on demand.
//
// Every host does this, for every name, which is the point: wildcard DNS
// points *.pilotrun.app at all of them, so a client can land anywhere and the
// host it lands on has to be able to complete the handshake. That works only
// because the certificate store is shared -- see internal/certs.
//
// Returns nil without serving when the fleet has no object storage or no ACME
// contact configured. A host that cannot share certificates must not obtain
// them: it would pass the handshake locally and fail on every other host,
// which is harder to diagnose than plain HTTP.
func startTLS(ctx context.Context, cfg *config.Config, store state.Store,
	objects *s3.Client, handler http.Handler) error {

	if objects == nil || cfg.ACMEEmail == "" {
		slog.Info("TLS is off; the router serves plain HTTP",
			"reason", "no object storage or no ACME contact configured")
		return nil
	}

	magic := certmagic.NewDefault()
	magic.Storage = certs.New(objects, cfg.HostID)

	// On demand, and refused for anything the fleet does not know. This
	// function runs on every handshake for an unrecognised SNI, so it is the
	// whole abuse defence -- see certs.Decider.
	decider := certs.NewDecider(store)
	magic.OnDemand = &certmagic.OnDemandConfig{
		DecisionFunc: func(ctx context.Context, name string) error {
			return decider.Allow(ctx, name)
		},
	}

	issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     certmagic.LetsEncryptProductionCA,
		Email:  cfg.ACMEEmail,
		Agreed: true,
		// TLS-ALPN cannot work here: the challenge would have to be answered
		// by whichever host the client resolved, and that host may not be the
		// one that started the order.
		DisableTLSALPNChallenge: true,
		// HTTP-01 stays on for custom domains, which cannot use DNS-01 -- the
		// zone belongs to the customer and we have no token for it.
		//
		// Distributed solvers stay ON. certmagic persists the challenge token
		// to Storage so any host can answer it -- without that, HTTP-01 fails
		// (N-1)/N of the time on an N-host fleet, and it looks like flaky
		// Let's Encrypt rather than a configuration mistake.
		//
		// DNS-01 is what obtains the wildcard, and it is the ONLY thing that
		// can: HTTP-01 cannot issue a wildcard certificate at all. nil when
		// no Cloudflare token is set, which certmagic reads as "this issuer
		// does not do DNS-01".
		DNS01Solver: dnsSolver(cfg),
	})
	magic.Issuers = []certmagic.Issuer{issuer}

	// The wildcard, both apexes, managed eagerly rather than on demand: an
	// on-demand certificate is obtained during a handshake, and the first
	// client to arrive would wait out a DNS-01 propagation check.
	//
	// Every host runs this identical call. That is not a race to be avoided,
	// it is the design: certs.Storage is shared and its lock serialises the
	// order, so ONE host obtains the certificate and the rest find it already
	// in storage. A fleet where only one host managed the wildcard would have
	// a host whose loss stops renewal.
	if names := wildcardNames(cfg); len(names) > 0 && cfg.CloudflareAPIToken != "" {
		go func() {
			if err := magic.ManageAsync(ctx, names); err != nil {
				slog.Error("could not manage the wildcard certificate; the router "+
					"will serve custom domains on demand and nothing else",
					"names", names, "err", err)
			}
		}()
		slog.Info("managing the wildcard certificate by ACME DNS-01", "names", names)
	} else {
		slog.Info("no Cloudflare API token; the router is HTTP-01-only and has no "+
			"wildcard certificate. Workload subdomains will fail the handshake "+
			"unless they are named in the fleet's state as custom domains",
			"env", "PILOT_CLOUDFLARE_API_TOKEN")
	}

	// Port 80 answers ACME challenges and redirects everything else. Every
	// host runs it because the challenge lands wherever DNS sent the client.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/", issuer.HTTPChallengeHandler(http.HandlerFunc(redirectToHTTPS)))
		srv := &http.Server{Addr: ":80", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
			slog.Error("the ACME challenge listener stopped; certificate issuance "+
				"and renewal will fail from now on", "err", err)
		}
	}()

	ln, err := net.Listen("tcp", ":443")
	if err != nil {
		return fmt.Errorf("hostd: listen on 443: %w", err)
	}
	tlsCfg := magic.TLSConfig()
	// http/1.1 alongside h2, so the exec stream's websocket upgrade keeps
	// working -- websockets are not defined over h2 here.
	tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)

	srv := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := srv.Serve(tls.NewListener(ln, tlsCfg)); err != nil && ctx.Err() == nil {
			slog.Error("the TLS listener stopped", "err", err)
		}
	}()
	slog.Info("serving TLS on :443 with on-demand certificates")
	return nil
}

// dnsSolver builds the ACME DNS-01 solver, or returns nil when the fleet has
// no Cloudflare token.
//
// Typed as acmez.Solver rather than *certmagic.DNS01Solver deliberately: a
// typed nil pointer in an interface field is not nil, and certmagic tests that
// field against nil to decide whether the issuer can do DNS-01 at all. Return
// the concrete type here and a token-less fleet would advertise a solver that
// panics on the first challenge.
func dnsSolver(cfg *config.Config) acmez.Solver {
	if cfg.CloudflareAPIToken == "" {
		return nil
	}
	return &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider: &cloudflare.Provider{APIToken: cfg.CloudflareAPIToken},
		},
	}
}

// wildcardNames is the set of names every host manages eagerly.
//
// The wildcard covers every workload subdomain, which includes the API
// hostname -- api.<workload domain> needs neither a record nor a certificate
// of its own. The workload apex is listed because a wildcard does not cover
// the apex it wildcards, and the dashboard apex because it is a separate zone
// (a guest sharing the dashboard's apex could set cookies scoped to it).
func wildcardNames(cfg *config.Config) []string {
	names := []string{"*." + cfg.WorkloadDomain, cfg.WorkloadDomain}
	if cfg.DashboardDomain != "" && cfg.DashboardDomain != cfg.WorkloadDomain {
		names = append(names, cfg.DashboardDomain)
	}
	return names
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
