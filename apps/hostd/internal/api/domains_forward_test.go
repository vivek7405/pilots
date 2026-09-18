package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Adding a domain through a host that is NOT the service's arbiter reaches the
// arbiter WITH ITS BODY.
//
// Only the arbiter may write the row, so every other host forwards. This
// handler is the one arbiter forward that reads the body first, to learn the
// service id, and it used to forward the drained request: the proxy announced
// a Content-Length and sent nothing, the transport broke the connection, and
// the caller saw "the host that writes this service is unreachable" for a host
// that was fine. DNS points the API name at every host, so on a three-host
// fleet two adds in three failed. The first production fleet found it.
//
// Counterfactual: drop the `r.Body = io.NopCloser(...)` line in handleAddDomain
// and the peer below reads an empty body.
func TestAddingADomainThroughANonArbiterForwardsTheBody(t *testing.T) {
	var got AddDomainRequest
	var raw []byte
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		if r.Header.Get(forwardedHeader) == "" {
			t.Error("the arbiter was not told the request had been forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(DomainResponse{Hostname: got.Hostname, ServiceID: got.ServiceID, Verified: true})
	}))
	defer peer.Close()

	_, st, fake := newTestServerWithManager(t)
	ctx := context.Background()
	// The only live host is the OTHER one, so it arbitrates every service.
	if err := st.PutHost(ctx, &state.Host{ID: "host-other", LastSeen: time.Now().Unix()}); err != nil {
		t.Fatalf("PutHost: %v", err)
	}
	if err := st.PutService(ctx, &state.Service{ID: "svc_web", Name: "web", Domain: "web", Replicas: 1}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "svc_web", OrgID: "org_1", Kind: "service"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	h := Routes(Deps{
		HostID: "host-test", Store: st, Machines: fake, Domain: "pilotrun.app",
		Peers: fakePeers{"host-other": peer.Listener.Addr().String()},
	})

	rec := doJSON(t, h, "POST", "/v1/domains", AddDomainRequest{ServiceID: "svc_web", Hostname: "example.com"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if got.ServiceID != "svc_web" || got.Hostname != "example.com" {
		t.Fatalf("the arbiter received %q; the body was drained before the forward", raw)
	}
	// And this host wrote nothing: the row is the arbiter's to write.
	if rows, _ := st.ListDomains(ctx); len(rows) != 0 {
		t.Errorf("this host wrote %d domain rows for a service it does not arbitrate", len(rows))
	}
}
