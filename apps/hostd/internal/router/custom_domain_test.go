package router

import (
	"context"
	"errors"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/machines"
	"github.com/pilotsrun/pilots/hostd/internal/state"
)

func shopRouter(custom func(string) (string, bool), ms ...state.Machine) *Router {
	return New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Name: "shop", Domain: "shop", ReleaseID: "rel-2"}},
			machines: ms,
		},
		CustomDomain: custom,
	})
}

// A verified custom hostname resolves to the service it was attached to.
//
// It never did. ParseHost refuses any name off the workload suffix, and resolve
// stopped there, so a custom domain was a row, a DNS check and a certificate,
// and then every request to it was unroutable. The production fleet's own
// pilots.run found it: the handshake succeeded and the API answered 401.
func TestACustomHostnameResolvesToItsService(t *testing.T) {
	r := shopRouter(
		func(host string) (string, bool) { return "shop", host == "shop.example.com" },
		state.Machine{ID: "m-old", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-a", State: "stopped"},
		state.Machine{ID: "m-new", ServiceID: "s-1", ReleaseID: "rel-2", HostID: "host-a", State: machines.StateRunning},
	)
	// The Host header as clients actually send it: any case, a port, a root dot.
	for _, host := range []string{"shop.example.com", "Shop.Example.COM", "shop.example.com:443", "shop.example.com."} {
		target, err := r.resolve(context.Background(), host)
		if err != nil {
			t.Fatalf("%q did not resolve: %v", host, err)
		}
		if target.Machine.ID != "m-new" {
			t.Errorf("%q routed to %q, want the current release's machine", host, target.Machine.ID)
		}
	}
}

// A hostname nobody verified is not ours, whatever it points at. Routing it
// would serve a tenant's service on a name someone else may own.
func TestAnUnknownHostnameOffTheSuffixDoesNotResolve(t *testing.T) {
	r := shopRouter(
		func(string) (string, bool) { return "", false },
		state.Machine{ID: "m-new", ServiceID: "s-1", ReleaseID: "rel-2", HostID: "host-a", State: machines.StateRunning},
	)
	if _, err := r.resolve(context.Background(), "evil.example.com"); err == nil {
		t.Fatal("a hostname no domain row names resolved to a machine")
	}
	// And with no index at all, as on a single box with no custom domains.
	if _, err := shopRouter(nil).resolve(context.Background(), "shop.example.com"); err == nil {
		t.Fatal("resolved a custom hostname with no index to resolve it from")
	}
}

// A custom hostname whose service has nothing on its current release is the
// service's 503, not an unknown host's 404: the URL is right and permanent.
func TestACustomHostnameWithNoReplicaIsNotAnUnknownHost(t *testing.T) {
	r := shopRouter(func(string) (string, bool) { return "shop", true })
	_, err := r.resolve(context.Background(), "shop.example.com")
	var none *noReplicaError
	if !errors.As(err, &none) {
		t.Fatalf("got %v, want the no-replica error a service's own address gives", err)
	}
}

// A custom hostname is a SERVICE's address. It must not be captured by a
// machine that happens to hold the same label as a name, which the workload
// suffix path allows on purpose.
func TestACustomHostnameIsNeverAMachineName(t *testing.T) {
	r := shopRouter(
		func(string) (string, bool) { return "shop", true },
		state.Machine{ID: "m-named", Name: "shop", HostID: "host-a", State: machines.StateRunning},
		state.Machine{ID: "m-replica", ServiceID: "s-1", ReleaseID: "rel-2", HostID: "host-a", State: machines.StateRunning},
	)
	target, err := r.resolve(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Machine.ID != "m-replica" {
		t.Errorf("routed to %q, want the service's replica", target.Machine.ID)
	}
}
