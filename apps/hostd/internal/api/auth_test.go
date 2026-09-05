package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// seedKey adds a key with the given org and scopes and returns its plaintext.
func seedKey(t *testing.T, st state.Store, key, org, scopes string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: org, Scopes: scopes,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	return key
}

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Scopes nest: machines is inside deploy is inside admin. A key must be
// refused on every route above its rank, and the refusal must name the scope
// the caller needs -- "forbidden" alone tells a client nothing it can act on.
func TestScopesAreEnforced(t *testing.T) {
	h, st := newTestServer(t)
	machinesKey := seedKey(t, st, "pilot_machines", "org_1", "machines")
	deployKey := seedKey(t, st, "pilot_deploy", "org_1", "deploy")

	for _, tc := range []struct {
		name, key, method, path string
		want                    int
		wantErr                 string
	}{
		{"machines key on builds", machinesKey, "POST", "/v1/builds", http.StatusForbidden, "scope deploy required"},
		{"machines key on services", machinesKey, "GET", "/v1/services", http.StatusForbidden, "scope deploy required"},
		{"machines key on api-keys", machinesKey, "POST", "/v1/api-keys", http.StatusForbidden, "scope admin required"},
		{"machines key on usage", machinesKey, "GET", "/v1/usage", http.StatusForbidden, "scope admin required"},
		{"machines key on its own routes", machinesKey, "GET", "/v1/machines", http.StatusOK, ""},
		// The stub's 503 is the proof the scope let it through: a 403 would
		// name the scope instead, and a 404 would mean the route was gone.
		{"machines key on the compose plan", machinesKey, "POST", "/v1/compose/plan", http.StatusServiceUnavailable, ""},
		{"deploy key on services", deployKey, "GET", "/v1/services", http.StatusOK, ""},
		{"deploy key on machines", deployKey, "GET", "/v1/machines", http.StatusOK, ""},
		{"deploy key on quotas", deployKey, "GET", "/v1/quotas/org_1", http.StatusForbidden, "scope admin required"},
		{"admin key on api-keys", testKey, "GET", "/v1/api-keys?org=org_1", http.StatusOK, ""},
		{"admin key on machines", testKey, "GET", "/v1/machines", http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, tc.key)
			if rec.Code != tc.want {
				t.Fatalf("%s %s: got %d, want %d (%s)", tc.method, tc.path,
					rec.Code, tc.want, rec.Body.String())
			}
			if tc.wantErr != "" && !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Errorf("body %s does not name the scope: want %q",
					rec.Body.String(), tc.wantErr)
			}
		})
	}
}

// A scope nothing recognises must reach nothing. The alternative -- treating
// an unknown name as harmless -- makes a typo in a key's scopes into a key
// that can do more than intended, not less.
func TestAnUnknownScopeFailsClosed(t *testing.T) {
	h, st := newTestServer(t)
	weird := seedKey(t, st, "pilot_weird", "org_1", "superuser")

	for _, path := range []string{"/v1/machines", "/v1/services", "/v1/api-keys"} {
		if rec := do(t, h, "GET", path, weird); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with an unknown scope: got %d, want 403", path, rec.Code)
		}
	}
}

// A route with no entry in the scope table needs admin. A route added without
// one is then reachable by the ops org alone until someone notices, rather
// than by every key on the fleet.
func TestAnUnmappedPathNeedsAdmin(t *testing.T) {
	h, st := newTestServer(t)
	machinesKey := seedKey(t, st, "pilot_machines", "org_1", "machines")

	if need, ok := scopeAllows("machines", "/v1/something-new"); ok || need != ScopeAdmin {
		t.Errorf("scopeAllows on an unmapped path = %q, %v; want admin, false", need, ok)
	}
	// End to end: an unmapped path is refused before the mux can 404 it.
	rec := do(t, h, "GET", "/v1/something-new", machinesKey)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rec.Code)
	}
}

// Revocation is checked on every request, from local state, so a key killed on
// one host stops working on all of them as the tombstone gossips out.
func TestARevokedKeyIsRefusedEverywhere(t *testing.T) {
	h, st := newTestServer(t)

	if rec := do(t, h, "GET", "/v1/machines", testKey); rec.Code != http.StatusOK {
		t.Fatalf("the key did not work before revoking: %d", rec.Code)
	}
	if err := st.PutRevocation(context.Background(),
		&state.Revocation{Hash: hashOf(testKey), RevokedAt: 1}); err != nil {
		t.Fatalf("PutRevocation: %v", err)
	}

	for _, path := range []string{"/v1/machines", "/v1/services", "/v1/hosts"} {
		if rec := do(t, h, "GET", path, testKey); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a revoked key: got %d, want 401", path, rec.Code)
		}
	}
	// The key row is untouched: revocation adds a tombstone rather than
	// deleting, because a delete loses to a replica carrying the insert.
	if _, err := st.GetAPIKeyByHash(context.Background(), hashOf(testKey)); err != nil {
		t.Errorf("revoking removed the key row: %v", err)
	}
}

// A browser cannot set an Authorization header on a WebSocket upgrade, so the
// key rides the subprotocol instead.
func TestTheKeyMayRideTheWebSocketSubprotocol(t *testing.T) {
	h, _ := newTestServer(t)

	for _, tc := range []struct {
		name, proto string
		want        int
	}{
		{"well formed", "authorization.bearer." + testKey, http.StatusOK},
		{"with other protocols offered", "chat, authorization.bearer." + testKey, http.StatusOK},
		{"empty key", "authorization.bearer.", http.StatusUnauthorized},
		{"wrong prefix", "authorization-bearer-" + testKey, http.StatusUnauthorized},
		{"bare key", testKey, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/machines", nil)
			req.Header.Set("Sec-WebSocket-Protocol", tc.proto)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("subprotocol %q: got %d, want %d", tc.proto, rec.Code, tc.want)
			}
		})
	}
}

// The header still wins when both are present, so an existing client that
// sends one is unaffected by the subprotocol carrier existing at all.
func TestTheHeaderIsStillHonoured(t *testing.T) {
	h, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/v1/machines", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Sec-WebSocket-Protocol", "authorization.bearer.pilot_wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
}

// The proxy echoes the entry the client OFFERED, not the key inside it: a
// handshake fails unless the server's choice is one of the offered values.
func TestBearerSubprotocolReturnsTheOfferedEntry(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
		ok                 bool
	}{
		{"one of several", "chat, authorization.bearer.x", "authorization.bearer.x", true},
		{"only entry", "authorization.bearer.pilot_k", "authorization.bearer.pilot_k", true},
		{"empty key", "authorization.bearer.", "", false},
		{"a bare key", "pilot_k", "", false},
		{"no header", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/machines/m_1/exec/stream", nil)
			if tc.header != "" {
				req.Header.Set("Sec-WebSocket-Protocol", tc.header)
			}
			got, ok := BearerSubprotocol(req)
			if ok != tc.ok || got != tc.want {
				t.Errorf("BearerSubprotocol = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The echo must cover an offer that carries no key: a client authenticating
// with the Authorization header may still offer a subprotocol, and a 101 that
// chose none fails that connection.
func TestOfferedSubprotocolReturnsTheFirstEntry(t *testing.T) {
	for _, tc := range []struct{ name, header, want string }{
		{"no header", "", ""},
		{"a name this server does not know", "pilots.v1", "pilots.v1"},
		{"the first of several", " pilots.v1 , chat ", "pilots.v1"},
		{"the bearer entry when it leads", "authorization.bearer.k, chat", "authorization.bearer.k"},
		{"the bearer entry when it does not", "chat, authorization.bearer.k", "chat"},
		{"empty entries are skipped", ", , pilots.v1", "pilots.v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/machines/m_1/exec/stream", nil)
			if tc.header != "" {
				req.Header.Set("Sec-WebSocket-Protocol", tc.header)
			}
			if got := OfferedSubprotocol(req); got != tc.want {
				t.Errorf("OfferedSubprotocol = %q; want %q", got, tc.want)
			}
		})
	}
}

// Every cross-host wake and suspend the autoscaler makes was a 401: the call
// carried the forwarding marker and no bearer, and the internal listener
// serves the same WithAuth-wrapped API a tenant reaches. The credential is
// derived from the secret that already unlocks every guest agent on the
// fleet, and it is inert from outside the mesh because the public listener
// strips the marker it needs.
func TestAPeerTokenAuthenticatesOnlyWithTheForwardingMarker(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	peer := PeerTokenFor("secret")

	withMarker := func(h http.Handler, key string, marked bool) int {
		req := httptest.NewRequest("GET", "/v1/machines", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if marked {
			req.Header.Set(forwardedHeader, "host-b")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	h := Routes(Deps{HostID: "host-a", Store: st, Machines: newFakeManager(), PeerToken: peer})
	if got := withMarker(h, peer, true); got != http.StatusOK {
		t.Errorf("a marked peer call got %d, want 200", got)
	}
	if got := withMarker(h, peer, false); got != http.StatusUnauthorized {
		t.Errorf("an unmarked peer token got %d, want 401", got)
	}
	if got := withMarker(h, PeerTokenFor("other"), true); got != http.StatusUnauthorized {
		t.Errorf("another fleet's peer token got %d, want 401", got)
	}

	// A box with no agent-token secret has no peers. Its peer token is empty,
	// which no bearer can equal -- WithAuth guards on that explicitly and
	// bearerToken refuses an empty bearer before it, so the hole is closed
	// twice. What actually keeps it closed is PeerTokenFor returning nothing
	// for an empty secret, which TestPeerTokenIsDerivedOnce pins.
	bare := Routes(Deps{HostID: "host-a", Store: st, Machines: newFakeManager(), PeerToken: PeerTokenFor("")})
	for _, key := range []string{"", "peer-", peer} {
		if got := withMarker(bare, key, true); got != http.StatusUnauthorized {
			t.Errorf("bearer %q against a box with no secret got %d, want 401", key, got)
		}
	}
}

// One derivation, so main.go and the peer caller cannot disagree about the
// credential -- and a label of its own, so the peer token space and the guest
// agent token space cannot collide.
func TestPeerTokenIsDerivedOnce(t *testing.T) {
	if PeerTokenFor("s") != PeerTokenFor("s") {
		t.Error("the derivation is not stable for one secret")
	}
	if PeerTokenFor("s") == PeerTokenFor("t") {
		t.Error("two secrets derived the same peer token")
	}
	if PeerTokenFor("") != "" {
		t.Error("a box with no secret got a peer token")
	}
	if !strings.HasPrefix(PeerTokenFor("s"), "peer-") {
		t.Errorf("peer token %q is not in its own namespace", PeerTokenFor("s"))
	}
}
