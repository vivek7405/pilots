package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// keyWithLimits mints a key row and its limits directly, which is what the
// OAuth token endpoint does through the API.
func keyWithLimits(t *testing.T, st state.Store, key, org string, l state.APIKeyLimits) {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	if err := st.PutAPIKey(context.Background(), &state.APIKey{Hash: hash, OrgID: org, Scopes: "deploy"}); err != nil {
		t.Fatal(err)
	}
	l.Hash = hash
	if l.Restricted() {
		if err := st.PutAPIKeyLimits(context.Background(), &l); err != nil {
			t.Fatal(err)
		}
	}
}

// An expired key authenticates nothing, on every route, and says so as a 401
// rather than a 403: the credential is finished, not merely too narrow.
func TestExpiredKeyIsRefusedEverywhere(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	const key = "pilot_expired"
	keyWithLimits(t, st, key, "org_1", state.APIKeyLimits{ExpiresAt: time.Now().Add(-time.Minute).Unix()})

	for _, path := range []string{"/v1/machines", "/v1/services", "/v1/whoami"} {
		rec := do(t, h, "GET", path, key)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with an expired key: got %d, want 401", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "expired") {
			t.Errorf("GET %s: the refusal does not say the token expired: %s", path, rec.Body.String())
		}
	}

	// A key whose expiry is still ahead of it works normally.
	const live = "pilot_still_good"
	keyWithLimits(t, st, live, "org_1", state.APIKeyLimits{ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if rec := do(t, h, "GET", "/v1/machines", live); rec.Code != http.StatusOK {
		t.Fatalf("a key with a future expiry: got %d, want 200", rec.Code)
	}
}

// The name prefix is enforced where a name is CHOSEN, and the refusal names
// the prefix so an agent can retry with a name that works.
func TestNamePrefixOnCreate(t *testing.T) {
	h, st, mgr := newTestServerWithManager(t)
	const key = "pilot_prefixed"
	keyWithLimits(t, st, key, "org_1", state.APIKeyLimits{NamePrefix: "mcp-"})

	rec := postJSON(t, h, "/v1/machines", key, `{"name":"production-db"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a name outside the prefix: got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error, "mcp-") || !strings.Contains(body.Next, "mcp-production-db") {
		t.Fatalf("the refusal does not name the prefix or a working name: %+v", body)
	}

	if rec := postJSON(t, h, "/v1/machines", key, `{"name":"mcp-scratch"}`); rec.Code != http.StatusCreated {
		t.Fatalf("a name inside the prefix: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	// An unnamed create is allowed, and is NAMED under the prefix. A minted
	// name outside it would be both outside the restriction and invisible to
	// the cap, which counts prefixed rows -- so the cap would never count
	// anything an agent made this way.
	if rec := postJSON(t, h, "/v1/machines", key, `{}`); rec.Code != http.StatusCreated {
		t.Fatalf("an unnamed create: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if name := mgr.lastCreate.Name; !strings.HasPrefix(name, "mcp-") {
		t.Fatalf("an unnamed create under a prefix reached the manager as %q, which is outside it", name)
	}

	// A service's name is what its permanent address is minted from, so the
	// same rule applies there.
	if rec := postJSON(t, h, "/v1/services", key, `{"name":"shop"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a service outside the prefix: got %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// An UNRESTRICTED key is untouched by any of it.
	if rec := postJSON(t, h, "/v1/machines", testKey, `{"name":"production-db"}`); rec.Code != http.StatusCreated {
		t.Fatalf("an unrestricted key: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// The cap counts the org's live machines carrying the prefix, and a destroyed
// one does not count: the row survives as a tombstone, and a cap that counted
// tombstones would shrink to zero over a day's work.
func TestMachineCap(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	const key = "pilot_capped"
	keyWithLimits(t, st, key, "org_1", state.APIKeyLimits{NamePrefix: "mcp-", MaxMachines: 2})

	ctx := context.Background()
	seed := func(id, name, machineState string) {
		if err := st.PutMachine(ctx, &state.Machine{ID: id, Name: name, HostID: "host-test", State: machineState}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutTenancy(ctx, &state.Tenancy{ID: id, OrgID: "org_1", Kind: "machine"}); err != nil {
			t.Fatal(err)
		}
	}
	seed("m_a", "mcp-one", "running")
	seed("m_b", "mcp-two", "destroyed")
	// Another org's machine with the same prefix must not count against this
	// token, and neither must a machine outside the prefix.
	seed("m_c", "production-db", "running")
	if err := st.PutMachine(ctx, &state.Machine{ID: "m_d", Name: "mcp-elsewhere", HostID: "host-test", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_d", OrgID: "org_2", Kind: "machine"}); err != nil {
		t.Fatal(err)
	}

	// One live prefixed machine, so a second is allowed.
	if rec := postJSON(t, h, "/v1/machines", key, `{"name":"mcp-two-of-two"}`); rec.Code != http.StatusCreated {
		t.Fatalf("under the cap: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// The cap refuses once the org is at it, with the numbers in the body.
func TestMachineCapRefuses(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	const key = "pilot_at_cap"
	keyWithLimits(t, st, key, "org_1", state.APIKeyLimits{NamePrefix: "mcp-", MaxMachines: 1})

	ctx := context.Background()
	if err := st.PutMachine(ctx, &state.Machine{ID: "m_a", Name: "mcp-one", HostID: "host-test", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_a", OrgID: "org_1", Kind: "machine"}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, h, "/v1/machines", key, `{"name":"mcp-two"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("at the cap: got %d, want 429: %s", rec.Code, rec.Body.String())
	}
	// The same refusal with NO name. An unnamed create used to be named by
	// hostd's own generator, outside the prefix, and the count above skips
	// rows outside the prefix -- so the cap was one omitted field away from
	// meaning nothing.
	if unnamed := postJSON(t, h, "/v1/machines", key, `{}`); unnamed.Code != http.StatusTooManyRequests {
		t.Fatalf("an unnamed create at the cap: got %d, want 429: %s", unnamed.Code, unnamed.Body.String())
	}
	var body struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != CodeQuotaExceeded {
		t.Errorf("code = %q, want %q", body.Code, CodeQuotaExceeded)
	}
	if body.Details["quota"] != "token_machines" || body.Details["scope"] != "token" {
		t.Errorf("details do not say the ceiling is the token's: %+v", body.Details)
	}
}

// The mint route takes the restrictions, writes them, and echoes them back on
// the mint and on the listing.
func TestMintWithRestrictions(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	expires := time.Now().Add(24 * time.Hour).Unix()

	rec := postJSON(t, h, "/v1/api-keys", testKey,
		`{"org_id":"org_agent","scopes":["machines"],"name_prefix":"mcp-","max_machines":5,"expires_at":`+itoa(int(expires))+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: got %d: %s", rec.Code, rec.Body.String())
	}
	var minted APIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.NamePrefix != "mcp-" || minted.MaxMachines != 5 || minted.ExpiresAt != expires {
		t.Fatalf("the mint did not echo the restrictions: %+v", minted)
	}

	stored, err := st.GetAPIKeyLimits(context.Background(), minted.Hash)
	if err != nil {
		t.Fatalf("the limits row was not written: %v", err)
	}
	if stored.NamePrefix != "mcp-" || stored.MaxMachines != 5 || stored.ExpiresAt != expires {
		t.Fatalf("stored = %+v", stored)
	}

	// Write-once: a second write cannot widen what a human approved.
	if err := st.PutAPIKeyLimits(context.Background(), &state.APIKeyLimits{Hash: minted.Hash, MaxMachines: 1000}); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetAPIKeyLimits(context.Background(), minted.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if again.MaxMachines != 5 || again.NamePrefix != "mcp-" {
		t.Fatalf("a second write changed the limits: %+v", again)
	}

	list := do(t, h, "GET", "/v1/api-keys?org=org_agent", testKey)
	if list.Code != http.StatusOK {
		t.Fatalf("list: got %d", list.Code)
	}
	var keys []APIKeyResponse
	if err := json.Unmarshal(list.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].NamePrefix != "mcp-" || keys[0].MaxMachines != 5 {
		t.Fatalf("the listing does not report the restrictions: %+v", keys)
	}
}

func TestMintRejectsImpossibleRestrictions(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	past := time.Now().Add(-time.Hour).Unix()
	for _, tc := range []struct{ name, body, want string }{
		{"a past expiry", `{"org_id":"o","scopes":["machines"],"expires_at":` + itoa(int(past)) + `}`, "past"},
		{"a negative cap", `{"org_id":"o","scopes":["machines"],"max_machines":-1}`, "negative"},
		{"an enormous prefix", `{"org_id":"o","scopes":["machines"],"name_prefix":"` + strings.Repeat("x", 41) + `"}`, "too long"},
	} {
		rec := postJSON(t, h, "/v1/api-keys", testKey, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %s", tc.name, rec.Body.String())
		}
	}
}
