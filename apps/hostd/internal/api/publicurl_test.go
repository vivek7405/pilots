package api

import "testing"

// TestPublicURLProductionShape is the pin. Before the derivation existed,
// handlers.go emitted the literal below unconditionally, and a host with
// object storage and PILOT_ACME_EMAIL -- which is every production host --
// must keep emitting it byte for byte.
//
// The listen address in the TLS case is deliberately :8080, because that is
// what production binds the plain listener to while the router serves TLS on
// :443. The port must not leak into the URL.
func TestPublicURLProductionShape(t *testing.T) {
	const want = "https://webapp.pilotrun.app"

	if got := PublicURLFor(true, ":8080").Of("webapp.pilotrun.app"); got != want {
		t.Errorf("TLS host renders %q, want %q", got, want)
	}
	// The zero value is the other half of the pin: a Deps built without a URL
	// field gets the production shape, which is why every existing test
	// construction stays as it is.
	if got := (PublicURL{}).Of("webapp.pilotrun.app"); got != want {
		t.Errorf("zero value renders %q, want %q", got, want)
	}
}

func TestPublicURLPlainHTTPOnAPort(t *testing.T) {
	const want = "http://webapp.pilots.localhost:8080"

	for _, addr := range []string{":8080", "0.0.0.0:8080"} {
		if got := PublicURLFor(false, addr).Of("webapp.pilots.localhost"); got != want {
			t.Errorf("listen %q renders %q, want %q", addr, got, want)
		}
	}
}

func TestPublicURLPlainHTTPOnEighty(t *testing.T) {
	// http already implies :80, so it is not spelled out.
	if got := PublicURLFor(false, ":80").Of("webapp.pilots.localhost"); got != "http://webapp.pilots.localhost" {
		t.Errorf("port 80 renders %q, want it dropped", got)
	}
	// An address the listener will reject anyway must not take the URL
	// derivation down with it; render the scheme and no port.
	if got := PublicURLFor(false, "nonsense").Of("webapp.pilots.localhost"); got != "http://webapp.pilots.localhost" {
		t.Errorf("unparsable listen address renders %q, want no port", got)
	}
}
