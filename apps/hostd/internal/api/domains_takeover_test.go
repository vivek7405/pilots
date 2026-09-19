package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// A hostname one service holds cannot be moved onto another by adding it again.
//
// PutDomain is an upsert keyed on the hostname, and an A record that matches
// the fleet's verifies for any name already pointing here, which every live
// custom domain does. So an add naming the caller's OWN service used to rewrite
// somebody else's verified row, and a verified row routes: the holder's traffic
// went to the caller's machines on the holder's certificate.
//
// Counterfactual: drop the GetDomain check in handleAddDomain and the second
// add answers 201 with the row pointing at svc_theirs.
func TestAHostnameAnotherServiceHoldsIsNotTakenOver(t *testing.T) {
	_, st, fake := newTestServerWithManager(t)
	ctx := context.Background()
	for _, id := range []string{"svc_mine", "svc_theirs"} {
		if err := st.PutService(ctx, &state.Service{ID: id, Name: id, Domain: id, Replicas: 1}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
	}
	if err := st.PutDomain(ctx, &state.Domain{
		Hostname: "shop.example.com", ServiceID: "svc_mine", VerifiedAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutDomain: %v", err)
	}
	// The name points at the fleet, as a live custom domain does, so the
	// A-record check passes whichever service the add names.
	fleet := []string{"203.0.113.7"}
	h := Routes(Deps{
		HostID: "host-test", Store: st, Machines: fake, Domain: "pilotrun.app",
		Resolver: fakeResolver{hosts: map[string][]string{
			"shop.example.com":        fleet,
			"svc_mine.pilotrun.app":   fleet,
			"svc_theirs.pilotrun.app": fleet,
		}},
	})

	rec := doJSON(t, h, "POST", "/v1/domains",
		AddDomainRequest{ServiceID: "svc_theirs", Hostname: "shop.example.com"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	row, err := st.GetDomain(ctx, "shop.example.com")
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if row.ServiceID != "svc_mine" || row.VerifiedAt == 0 {
		t.Fatalf("the row now reads %+v; the hostname was taken from its holder", row)
	}

	// Adding it again to the service that already holds it is still an add.
	rec = doJSON(t, h, "POST", "/v1/domains",
		AddDomainRequest{ServiceID: "svc_mine", Hostname: "shop.example.com"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a re-add by the holder got %d: %s", rec.Code, rec.Body.String())
	}
}
