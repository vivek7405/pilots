package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

const fleetSecret = "a-fleet-agent-token-secret"

// sealer is the fleet key, faked the way the api tests fake it: Open is Seal's
// inverse, and both fail when the key is not set, so a test that drops the key
// sees the same refusal a host without one would give.
type sealer struct{ set bool }

func (s sealer) IsSet() bool { return s.set }
func (s sealer) Seal(raw []byte) (string, error) {
	return "sealed:" + string(raw), nil
}
func (s sealer) Open(blob string) ([]byte, error) {
	if !s.set {
		return nil, errNoKey
	}
	return []byte(strings.TrimPrefix(blob, "sealed:")), nil
}

var errNoKey = &keyErr{}

type keyErr struct{}

func (*keyErr) Error() string { return "no fleet key" }

type tenancy map[string]string

func (t tenancy) OrgOf(_ context.Context, id string) (string, bool) {
	org, ok := t[id]
	return org, ok
}

// serverFor builds a broker over a real in-memory store, so the resolution
// order is exercised against the same queries production runs.
func serverFor(t *testing.T, key bool) (*Server, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_1", Name: "sandbox", State: state.StateRunning, ServiceID: "s_1",
	}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	return New(Options{
		Store: st, Seal: sealer{set: key},
		Tenant: tenancy{"m_1": "org_1"},
		Key:    api.BrokerKeyFor(fleetSecret),
		APIURL: "https://api.example.test",
	}), st
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.HandlerFor("m_1").ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// Deny by default is the whole shape. A machine nobody granted anything to
// reaches nothing, and that is the NORMAL state rather than an error path.
func TestWithNoGrantAMachineGetsNothing(t *testing.T) {
	s, _ := serverFor(t, true)
	for _, path := range []string{"/token", "/secrets"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 with no grant", path, rec.Code)
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["code"] != "no_grant" {
			t.Errorf("%s code = %q, want no_grant so a client can branch on it", path, body["code"])
		}
	}
}

// Knowing your own id is not a credential, so identity answers without one.
func TestIdentityAnswersWithNoGrant(t *testing.T) {
	s, _ := serverFor(t, true)
	rec := get(t, s, "/identity")
	if rec.Code != http.StatusOK {
		t.Fatalf("identity = %d %s", rec.Code, rec.Body.String())
	}
	var got IdentityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MachineID != "m_1" || got.OrgID != "org_1" || got.ServiceID != "s_1" {
		t.Errorf("identity = %+v", got)
	}
	if got.APIURL == "" {
		t.Error("no api_url, so a guest would have to construct one and could construct it wrongly")
	}
}

func TestAGrantedMachineMintsATokenScopedToItself(t *testing.T) {
	s, st := serverFor(t, true)
	mustGrant(t, st, &state.BrokerGrant{
		ID: "m_1", Kind: "machine", OrgID: "org_1", Scopes: []string{api.ScopeMachines},
	})

	rec := get(t, s, "/token")
	if rec.Code != http.StatusOK {
		t.Fatalf("token = %d %s", rec.Code, rec.Body.String())
	}
	var got TokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	claims, err := api.VerifyBrokerToken(api.BrokerKeyFor(fleetSecret), got.Token)
	if err != nil {
		t.Fatalf("the broker minted a token this fleet does not accept: %v", err)
	}
	if claims.Machine != "m_1" || claims.Org != "org_1" || claims.Service != "s_1" {
		t.Errorf("claims = %+v, want this machine, its org and its service", claims)
	}
}

// A scope that was not granted must be refused by NAME rather than quietly
// dropped: a client handed a narrower token than it asked for fails later, at
// the call, with nothing saying why.
func TestAScopeThatWasNotGrantedIsRefused(t *testing.T) {
	s, st := serverFor(t, true)
	mustGrant(t, st, &state.BrokerGrant{
		ID: "m_1", Kind: "machine", OrgID: "org_1", Scopes: []string{api.ScopeMachines},
	})
	rec := get(t, s, "/token?scope="+api.ScopeDeploy)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token?scope=deploy = %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// A service's replicas are recreated on every deploy, so a grant written per
// replica could never serve the PaaS face. The service's grant has to reach
// them, and the machine's own has to win where both exist.
func TestAServiceGrantReachesItsReplicasAndTheMachinesOwnWins(t *testing.T) {
	s, st := serverFor(t, true)
	mustGrant(t, st, &state.BrokerGrant{
		ID: "s_1", Kind: "service", OrgID: "org_1", Scopes: []string{api.ScopeMachines},
	})

	rec := get(t, s, "/token")
	if rec.Code != http.StatusOK {
		t.Fatalf("a replica did not inherit its service's grant: %d %s", rec.Code, rec.Body.String())
	}

	// Now the machine's own, which is the more specific statement and was
	// written deliberately, so it wins.
	mustGrant(t, st, &state.BrokerGrant{
		ID: "m_1", Kind: "machine", OrgID: "org_1", Scopes: []string{},
	})
	rec = get(t, s, "/token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token = %d, want 403: the machine's own empty grant must beat "+
			"its service's", rec.Code)
	}
}

func TestSecretsComeBackOpenedAndOnlyWithAKey(t *testing.T) {
	s, st := serverFor(t, true)
	mustGrant(t, st, &state.BrokerGrant{
		ID: "m_1", Kind: "machine", OrgID: "org_1",
		Sealed: `sealed:{"DATABASE_URL":"postgres://x"}`,
	})
	rec := get(t, s, "/secrets")
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets = %d %s", rec.Code, rec.Body.String())
	}
	var got SecretsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Secrets["DATABASE_URL"] != "postgres://x" {
		t.Errorf("secrets = %v", got.Secrets)
	}

	// The same rows on a host with no fleet key must SAY so rather than answer
	// with an empty map, which a guest would read as "there are no secrets".
	keyless := New(Options{
		Store: st, Seal: sealer{set: false}, Tenant: tenancy{"m_1": "org_1"},
		Key: api.BrokerKeyFor(fleetSecret),
	})
	rec = httptest.NewRecorder()
	keyless.HandlerFor("m_1").ServeHTTP(rec, httptest.NewRequest("GET", "/secrets", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("secrets on a keyless host = %d, want 503", rec.Code)
	}
}

// Destroying a machine ends its credentials at once. The namespace can outlive
// the row by the length of a teardown, which is exactly the window this closes.
func TestADestroyedMachineBrokersNothing(t *testing.T) {
	s, st := serverFor(t, true)
	mustGrant(t, st, &state.BrokerGrant{
		ID: "m_1", Kind: "machine", OrgID: "org_1", Scopes: []string{api.ScopeMachines},
	})
	m, err := st.GetMachine(context.Background(), "m_1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	m.State = state.StateDestroyed
	if err := st.PutMachine(context.Background(), m); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	if rec := get(t, s, "/token"); rec.Code != http.StatusForbidden {
		t.Errorf("a destroyed machine minted a token: %d", rec.Code)
	}
	if rec := get(t, s, "/secrets"); rec.Code != http.StatusForbidden {
		t.Errorf("a destroyed machine read its secrets: %d", rec.Code)
	}
}

// An empty scope list is a grant that exists and permits no token. It must not
// read as "grant everything", which is what an absent check would make it.
func TestAGrantWithNoScopesMintsNothing(t *testing.T) {
	s, st := serverFor(t, true)
	mustGrant(t, st, &state.BrokerGrant{
		ID: "m_1", Kind: "machine", OrgID: "org_1", Scopes: []string{},
		Sealed: `sealed:{"A":"1"}`,
	})
	if rec := get(t, s, "/token"); rec.Code != http.StatusForbidden {
		t.Errorf("token = %d, want 403 on a grant that carries no scopes", rec.Code)
	}
	// The secrets half of the same grant still works: the two are granted
	// separately and one being empty says nothing about the other.
	if rec := get(t, s, "/secrets"); rec.Code != http.StatusOK {
		t.Errorf("secrets = %d, want 200: scopes and secrets are granted separately", rec.Code)
	}
}

func mustGrant(t *testing.T, st state.Store, g *state.BrokerGrant) {
	t.Helper()
	if err := st.PutBrokerGrant(context.Background(), g); err != nil {
		t.Fatalf("PutBrokerGrant: %v", err)
	}
}
