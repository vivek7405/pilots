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
		// HTTP-01 only. TLS-ALPN cannot work here: the challenge would have to
		// be answered by whichever host the client resolved, and that host may
		// not be the one that started the order.
		DisableTLSALPNChallenge: true,
		// Distributed solvers stay ON. certmagic persists the challenge token
		// to Storage so any host can answer it -- without that, HTTP-01 fails
		// (N-1)/N of the time on an N-host fleet, and it looks like flaky
		// Let's Encrypt rather than a configuration mistake.
	})
	magic.Issuers = []certmagic.Issuer{issuer}

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

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
