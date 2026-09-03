package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func postJSON(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A minted key must authenticate, its hash must be the sha256 of the
// plaintext, and the plaintext must appear exactly once -- on the mint.
func TestMintedKeyAuthenticates(t *testing.T) {
	h, _ := newTestServer(t)

	rec := postJSON(t, h, "/v1/api-keys", testKey,
		`{"org_id":"org_new","scopes":["machines","deploy"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: got %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var minted APIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(minted.Key, KeyPrefix) {
		t.Errorf("key %q does not start with %q", minted.Key, KeyPrefix)
	}
	if hashOf(minted.Key) != minted.Hash {
		t.Errorf("hash %q is not the sha256 of the key", minted.Hash)
	}
	if minted.OrgID != "org_new" || len(minted.Scopes) != 2 {
		t.Errorf("read back %+v", minted)
	}

	if got := do(t, h, "GET", "/v1/machines", minted.Key); got.Code != http.StatusOK {
		t.Errorf("the minted key does not authenticate: %d", got.Code)
	}
	// It carries deploy, not admin.
	if got := do(t, h, "GET", "/v1/api-keys?org=org_new", minted.Key); got.Code != http.StatusForbidden {
		t.Errorf("a deploy key reached an admin route: %d", got.Code)
	}

	// The list never carries a plaintext, because none was stored.
	list := do(t, h, "GET", "/v1/api-keys?org=org_new", testKey)
	if strings.Contains(list.Body.String(), minted.Key) {
		t.Errorf("the key list leaked the plaintext: %s", list.Body.String())
	}
}

func TestRevokeKillsTheKeyAndShowsInTheList(t *testing.T) {
	h, _ := newTestServer(t)

	rec := postJSON(t, h, "/v1/api-keys", testKey, `{"org_id":"org_new","scopes":["machines"]}`)
	var minted APIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := do(t, h, "GET", "/v1/machines", minted.Key); got.Code != http.StatusOK {
		t.Fatalf("the key did not work before revoking: %d", got.Code)
	}

	rev := postJSON(t, h, "/v1/api-keys/"+minted.Hash+"/revoke", testKey, "")
	if rev.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200 (%s)", rev.Code, rev.Body.String())
	}
	if got := do(t, h, "GET", "/v1/machines", minted.Key); got.Code != http.StatusUnauthorized {
		t.Errorf("the revoked key still authenticates: %d", got.Code)
	}
	// Idempotent: revoking again is the same operation with the same effect.
	if again := postJSON(t, h, "/v1/api-keys/"+minted.Hash+"/revoke", testKey, ""); again.Code != http.StatusOK {
		t.Errorf("re-revoking: got %d, want 200", again.Code)
	}
	// An unknown hash is still 200: refusing would make the route an oracle
	// for which hashes exist.
	if unknown := postJSON(t, h, "/v1/api-keys/deadbeef/revoke", testKey, ""); unknown.Code != http.StatusOK {
		t.Errorf("revoking an unknown hash: got %d, want 200", unknown.Code)
	}

	list := do(t, h, "GET", "/v1/api-keys?org=org_new", testKey)
	var rows []APIKeyResponse
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].RevokedAt == 0 {
		t.Errorf("the list does not report the revocation: %+v", rows)
	}
}

// A revoked hash is dead forever. Re-minting one would produce a key that
// looks valid and authenticates nowhere, because every host would refuse it
// on the tombstone it already holds.
func TestARevokedHashCannotBeReminted(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_ops", "admin")

	// A fixed key source, so the test knows the hash the next mint produces.
	seed := strings.Repeat("a", 64)
	_, hash, err := MintKey(strings.NewReader(seed))
	if err != nil {
		t.Fatalf("MintKey: %v", err)
	}
	if err := st.PutRevocation(ctx, &state.Revocation{Hash: hash, RevokedAt: 1}); err != nil {
		t.Fatalf("PutRevocation: %v", err)
	}

	h := Routes(Deps{HostID: "host-test", Store: st, Machines: newFakeManager(),
		KeySource: strings.NewReader(seed)})

	rec := postJSON(t, h, "/v1/api-keys", testKey, `{"org_id":"org_new","scopes":["machines"]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-minting a revoked hash: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	// And nothing was written: a refused mint must leave no key row behind.
	if _, err := st.GetAPIKeyByHash(ctx, hash); err == nil {
		t.Error("the refused mint wrote a key row anyway")
	}
}

func TestMintValidatesItsBody(t *testing.T) {
	h, st := newTestServer(t)
	machinesKey := seedKey(t, st, "pilot_machines", "org_1", "machines")

	for _, tc := range []struct {
		name, key, body string
		want            int
	}{
		{"no org", testKey, `{"scopes":["machines"]}`, http.StatusBadRequest},
		{"no scopes", testKey, `{"org_id":"org_new"}`, http.StatusBadRequest},
		{"unknown scope", testKey, `{"org_id":"org_new","scopes":["root"]}`, http.StatusBadRequest},
		{"non-admin", machinesKey, `{"org_id":"org_new","scopes":["machines"]}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postJSON(t, h, "/v1/api-keys", tc.key, tc.body); rec.Code != tc.want {
				t.Errorf("got %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	if rec := do(t, h, "GET", "/v1/api-keys", testKey); rec.Code != http.StatusBadRequest {
		t.Errorf("listing with no org: got %d, want 400", rec.Code)
	}
}
