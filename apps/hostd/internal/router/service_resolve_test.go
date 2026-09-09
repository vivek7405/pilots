package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/machines"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The whole point of the issue this file was written for: a service's address
// is permanent, and a deploy replaces its machines. So the address must
// resolve to whatever the CURRENT release is, and never to the previous
// release's machines, which a blue/green deploy stops but keeps for rollback.
func TestResolveServesAServiceAddressFromItsCurrentRelease(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Name: "shop", Domain: "shop", ReleaseID: "rel-2"}},
			machines: []state.Machine{
				{ID: "m-old", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-a", State: "stopped"},
				{ID: "m-new", ServiceID: "s-1", ReleaseID: "rel-2", HostID: "host-a", State: machines.StateRunning},
			},
		},
	})

	target, err := r.resolve(context.Background(), "shop.pilotrun.app")
	if err != nil {
		t.Fatalf("a service address did not resolve: %v", err)
	}
	if target.Machine.ID != "m-new" {
		t.Errorf("routed to %q, want the current release's machine m-new", target.Machine.ID)
	}
}

// Machine names and service addresses share one namespace. Both allocators
// refuse a label the other holds, so this can only happen through a cross-host
// race, and every host has to break the tie the same way or one URL means two
// different things depending on which host the client reached.
func TestResolvePrefersAMachineNameOverAServiceAddress(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: "rel-1"}},
			machines: []state.Machine{
				{ID: "m-named", Name: "shop", HostID: "host-a", State: machines.StateRunning},
				{ID: "m-replica", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-a", State: machines.StateRunning},
			},
		},
	})

	target, err := r.resolve(context.Background(), "shop.pilotrun.app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Machine.ID != "m-named" {
		t.Errorf("routed to %q, want the machine that holds the name", target.Machine.ID)
	}
}

// A request that arrived here should be served here rather than forwarded over
// the mesh, so a local running replica wins every time and not merely usually.
func TestResolvePrefersALocalRunningReplica(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: "rel-1"}},
			machines: []state.Machine{
				{ID: "m-remote", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-b", State: machines.StateRunning},
				{ID: "m-local", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-a", State: machines.StateRunning},
			},
		},
	})

	for i := 0; i < 50; i++ {
		target, err := r.resolve(context.Background(), "shop.pilotrun.app")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if target.Machine.ID != "m-local" {
			t.Fatalf("try %d routed to %q, want the local replica", i, target.Machine.ID)
		}
	}
}

// A service whose replicas are all suspended is not down, it is asleep. The
// router returns one anyway so ensureAwake can wake it while holding the
// request, which is what a machine reached by its own name already gets.
func TestResolveWakesASuspendedReplicaWhenNoneRuns(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: "rel-1"}},
			machines: []state.Machine{
				{ID: "m-asleep", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-a", State: "suspended"},
			},
		},
	})

	target, err := r.resolve(context.Background(), "shop.pilotrun.app")
	if err != nil {
		t.Fatalf("a suspended replica did not resolve: %v", err)
	}
	if target.Machine.ID != "m-asleep" {
		t.Errorf("routed to %q, want the suspended replica", target.Machine.ID)
	}
}

// The two failures are different facts and a caller acts on them differently.
// An address that exists but has never been deployed is the service's own
// state; telling the caller their URL is unknown would be a lie about a URL
// that is permanent and correct.
func TestAServiceWithNoReplicasIs503NotUnknownHost(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: ""}},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://shop.pilotrun.app/", nil)
	req.Host = "shop.pilotrun.app"
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a service with no release answered %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no release yet") {
		t.Errorf("body does not say why: %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://nobody.pilotrun.app/", nil)
	req.Host = "nobody.pilotrun.app"
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("an unknown label answered %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown host") {
		t.Errorf("body does not say unknown host: %q", rec.Body.String())
	}
}

// Routing is the hot path, so a store read per request is a round trip to the
// corrosion agent per request. The subscription cache answers first, and the
// panic proves it rather than assuming it.
func TestResolveUsesTheServiceLookupBeforeTheStore(t *testing.T) {
	replica := state.Machine{
		ID: "m-1", ServiceID: "s-1", ReleaseID: "rel-1",
		HostID: "host-a", State: machines.StateRunning,
	}
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{servicesPanic: true},
		Service: func(label string) (state.Service, []state.Machine, bool) {
			if label != "shop" {
				return state.Service{}, nil, false
			}
			return state.Service{ID: "s-1", Domain: "shop", ReleaseID: "rel-1"},
				[]state.Machine{replica}, true
		},
	})

	target, err := r.resolve(context.Background(), "shop.pilotrun.app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Machine.ID != "m-1" {
		t.Errorf("routed to %q, want m-1", target.Machine.ID)
	}
}

// The port-prefix form is how a caller reaches any port without the platform
// knowing about it in advance. It is parsed before anything looks at what kind
// of thing holds the label, so a service gets it for free -- but free is not
// the same as proven.
func TestAServiceAddressTakesThePortPrefixForm(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: "rel-1"}},
			machines: []state.Machine{
				{ID: "m-1", ServiceID: "s-1", ReleaseID: "rel-1", HostID: "host-a", State: machines.StateRunning},
			},
		},
	})

	target, err := r.resolve(context.Background(), "3000-shop.pilotrun.app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Machine.ID != "m-1" || target.Port != 3000 {
		t.Errorf("routed to %q port %d, want m-1 port 3000", target.Machine.ID, target.Port)
	}
}

// The cache holding the service and none of its machines is not proof the
// service has none. machines and services arrive on two subscriptions and
// nothing orders one against the other, so a host can hold the flipped
// release_id before it holds the machines that release created. Answering 503
// there would take a live service down on that host for as long as the lag
// lasts, when the local store -- read at the moment it is asked -- has the
// rows.
func TestAReplicaLessCacheHitFallsBackToTheStore(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{
			services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: "rel-2"}},
			machines: []state.Machine{
				{ID: "m-new", ServiceID: "s-1", ReleaseID: "rel-2", HostID: "host-a", State: machines.StateRunning},
			},
		},
		// The service row, with the release the store already agrees on, and
		// not one machine of it.
		Service: func(label string) (state.Service, []state.Machine, bool) {
			if label != "shop" {
				return state.Service{}, nil, false
			}
			return state.Service{ID: "s-1", Domain: "shop", ReleaseID: "rel-2"}, nil, true
		},
	})

	target, err := r.resolve(context.Background(), "shop.pilotrun.app")
	if err != nil {
		t.Fatalf("a lagging cache turned a live service into an error: %v", err)
	}
	if target.Machine.ID != "m-new" {
		t.Errorf("routed to %q, want the store's m-new", target.Machine.ID)
	}
}

// The other half of the same rule: when the store agrees there is nothing to
// serve, the answer is still 503 rather than 404. The address is real.
func TestAReplicaLessServiceStaysA503AfterTheStoreAgrees(t *testing.T) {
	r := New(Options{
		Domain: "pilotrun.app", HostID: "host-a",
		Store: &stubStore{services: []state.Service{{ID: "s-1", Domain: "shop", ReleaseID: ""}}},
		Service: func(label string) (state.Service, []state.Machine, bool) {
			if label != "shop" {
				return state.Service{}, nil, false
			}
			return state.Service{ID: "s-1", Domain: "shop"}, nil, true
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://shop.pilotrun.app/", nil)
	req.Host = "shop.pilotrun.app"
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("answered %d, want 503", rec.Code)
	}
}
