package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func readEgress(t *testing.T, h http.Handler) EgressResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/egress", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/egress: %d: %s", rec.Code, rec.Body.String())
	}
	var out EgressResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// The counterfactual for the whole feature. A fleet where no host has been
// given a prefix answers with an empty set, not an error and not an invented
// address: every machine leaves from its host's shared address, which is what
// they did before this existed.
func TestAFleetWithNoEgressPrefixReportsNoAddresses(t *testing.T) {
	h, _ := newTestServer(t)

	got := readEgress(t, h)
	if len(got.Addresses) != 0 {
		t.Errorf("addresses = %+v, want none on a fleet that manages no egress", got.Addresses)
	}
	if got.OrgID == "" {
		t.Error("the response does not say whose addresses these are")
	}
}

// One address per host, because the address is derived from the HOST's prefix.
// A tenant allowlists the set; it changes when a host joins or leaves and at no
// other time.
func TestEgressReportsOneAddressPerHost(t *testing.T) {
	h, st := newTestServer(t)
	ctx := t.Context()

	for _, row := range []state.HostEgress{
		{HostID: "host-a", Prefix6: "2a01:4f8:1c17:1::/64", Interface: "eth0"},
		{HostID: "host-b", Prefix6: "2a01:4f8:1c17:2::/64", Interface: "eth0"},
	} {
		row.UpdatedAt = time.Now().Unix()
		if err := st.PutHostEgress(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}

	got := readEgress(t, h)
	if len(got.Addresses) != 2 {
		t.Fatalf("addresses = %+v, want one per host", got.Addresses)
	}
	if got.Addresses[0].IPv6 == got.Addresses[1].IPv6 {
		t.Error("two hosts reported the same address; each derives from its own prefix")
	}
	for _, a := range got.Addresses {
		addr, err := netip.ParseAddr(a.IPv6)
		if err != nil {
			t.Fatalf("host %s reported %q, which is not an address: %v", a.HostID, a.IPv6, err)
		}
		if !addr.Is6() {
			t.Errorf("host %s reported %s, which is not IPv6", a.HostID, addr)
		}
	}
}

// A host whose row cannot be read as a prefix is SKIPPED, not reported with a
// broken address. Handing a tenant something to put in another party's
// firewall is a promise, and one derived from a prefix that does not parse is
// a promise this fleet cannot keep.
func TestAHostWithAnUnreadablePrefixIsLeftOut(t *testing.T) {
	h, st := newTestServer(t)

	if err := st.PutHostEgress(t.Context(), &state.HostEgress{
		HostID: "host-broken", Prefix6: "not a prefix", UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	got := readEgress(t, h)
	if len(got.Addresses) != 0 {
		t.Errorf("addresses = %+v, want none: the one host's prefix is unreadable", got.Addresses)
	}
}

// The same org asking twice gets the same answer, which is the entire point:
// an address that moved would have to be re-allowlisted, and re-allowlisting
// is the cost this feature exists to remove.
func TestTheReportedAddressDoesNotMove(t *testing.T) {
	h, st := newTestServer(t)

	if err := st.PutHostEgress(t.Context(), &state.HostEgress{
		HostID: "host-a", Prefix6: "2a01:4f8:1c17:1::/64", UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	first := readEgress(t, h)
	if len(first.Addresses) != 1 {
		t.Fatalf("addresses = %+v, want one", first.Addresses)
	}
	for i := 0; i < 5; i++ {
		again := readEgress(t, h)
		if again.Addresses[0].IPv6 != first.Addresses[0].IPv6 {
			t.Fatalf("read %d gave %s, first gave %s",
				i, again.Addresses[0].IPv6, first.Addresses[0].IPv6)
		}
	}
}
