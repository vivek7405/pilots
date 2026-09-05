package api

import "net"

// PublicURL is how a hostname becomes the URL a client is told: the scheme,
// and the port when the fleet listens on one a browser would not assume.
//
// Decided ONCE at startup from whether TLS started (see cmd/hostd), never per
// request: a machine's URL is a property of the fleet, not of which listener
// the caller happened to use. A peer forwarding over the mesh asks over plain
// HTTP on the internal listener, and the answer must not change because of it.
//
// The zero value renders the production shape -- https, no port -- so a test
// that does not care about the scheme gets exactly what a TLS host emits.
type PublicURL struct {
	Scheme string // "https" or "http"; empty means https
	Port   string // appended as ":port"; empty means none
}

// Of renders host as a URL a client can open.
func (u PublicURL) Of(host string) string {
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if u.Port == "" {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + u.Port
}

// PublicURLFor derives the shape from the two facts that decide it. With TLS
// the router serves :443 whatever the plain listener is bound to, so the port
// is dropped; without it the plain listener's port is the only way in, and it
// is dropped only when it is the one http already implies.
func PublicURLFor(tlsOn bool, listenAddr string) PublicURL {
	if tlsOn {
		return PublicURL{Scheme: "https"}
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "80" {
		return PublicURL{Scheme: "http"}
	}
	return PublicURL{Scheme: "http", Port: port}
}
