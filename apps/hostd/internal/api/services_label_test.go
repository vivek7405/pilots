package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// labelServer is serviceServer with the control API's hostname configured and
// the store handed back, so a test can seed a machine or a second service into
// the namespace the allocator scans.
func labelServer(t *testing.T) (http.Handler, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")

	ctx := context.Background()
	if err := st.PutService(ctx, &state.Service{
		ID: "s_1", Name: "web", Replicas: 1, Domain: "web", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{
		ID: "s_1", OrgID: "org_1", Kind: "service", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	return Routes(Deps{
		HostID: "host-test", Store: st, Domain: "pilotrun.app",
		APIHostname: "api.pilotrun.app", FleetKey: fakeSealer{set: true},
	}), st
}

func createdService(t *testing.T, rec interface{ Bytes() []byte }) Service {
	t.Helper()
	var got Service
	if err := json.Unmarshal(rec.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// The issue in one assertion: a service that nobody remembered to give an
// address still has one, and it is the name they already know.
func TestCreateMintsTheNameAsTheAddress(t *testing.T) {
	h, st := labelServer(t)

	rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
		"name": "shop", "app": "storefront", "replicas": 1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body)
	}
	got := createdService(t, rec.Body)
	if !strings.HasSuffix(got.URL, "shop.pilotrun.app") {
		t.Errorf("url = %q, want it to end in shop.pilotrun.app", got.URL)
	}

	row, err := st.GetService(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if row.Domain != "shop" {
		t.Errorf("stored domain = %q, want shop", row.Domain)
	}
}

// A name is a person's words, so it may not be a legal label at all. The
// address is normalised rather than refused, because refusing would make a
// service name a DNS name, which it is not.
func TestCreateNormalisesAnAwkwardNameIntoAnAddress(t *testing.T) {
	h, _ := labelServer(t)

	rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
		"name": "My Shop", "app": "storefront", "replicas": 1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body)
	}
	if got := createdService(t, rec.Body); !strings.HasSuffix(got.URL, "my-shop.pilotrun.app") {
		t.Errorf("url = %q, want it to end in my-shop.pilotrun.app", got.URL)
	}
}

// Machine names and service addresses are one namespace, so the allocator has
// to see both. Taking a machine's name would take its URL, which is permanent.
func TestCreateMintsASuffixWhenTheNameIsTaken(t *testing.T) {
	suffixed := regexp.MustCompile(`^shop-[a-z0-9]{4}\.pilotrun\.app$`)

	t.Run("held by a machine", func(t *testing.T) {
		h, st := labelServer(t)
		if err := st.PutMachine(context.Background(), &state.Machine{
			ID: "m_1", Name: "shop", HostID: "host-test", State: "running",
		}); err != nil {
			t.Fatalf("PutMachine: %v", err)
		}

		rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
			"name": "shop", "app": "storefront", "replicas": 1,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body)
		}
		got := createdService(t, rec.Body)
		host := strings.TrimPrefix(strings.TrimPrefix(got.URL, "https://"), "http://")
		if !suffixed.MatchString(host) {
			t.Errorf("url host = %q, want shop with a four-character suffix", host)
		}
	})

	t.Run("held by another service", func(t *testing.T) {
		h, _ := labelServer(t)
		// s_1 already holds "web".
		rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
			"name": "web", "app": "storefront", "replicas": 1,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body)
		}
		got := createdService(t, rec.Body)
		host := strings.TrimPrefix(strings.TrimPrefix(got.URL, "https://"), "http://")
		if !regexp.MustCompile(`^web-[a-z0-9]{4}\.pilotrun\.app$`).MatchString(host) {
			t.Errorf("url host = %q, want web with a four-character suffix", host)
		}
	})
}

// An explicit address is never adjusted. A caller who asked for one is going
// to hard code it, so silently serving them a different label is worse than a
// refusal they can act on.
func TestAnExplicitAddressIsTakenLiterallyOrRefused(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		status       int
		says         string
	}{
		{"free", "brandnew", http.StatusCreated, ""},
		{"taken by a service", "web", http.StatusConflict, "already taken"},
		{"not a label", "Has.Dot", http.StatusBadRequest, "dot"},
		{"a port selector", "8080-api", http.StatusBadRequest, "port"},
		{"the control API", "api", http.StatusBadRequest, "control API"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := labelServer(t)
			rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
				"name": "svc", "app": "storefront", "replicas": 1, "domain": tc.domain,
			})
			if rec.Code != tc.status {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.says != "" && !strings.Contains(rec.Body.String(), tc.says) {
				t.Errorf("the refusal does not say why: %s", rec.Body)
			}
			if tc.status == http.StatusCreated {
				if got := createdService(t, rec.Body); !strings.HasSuffix(got.URL, tc.domain+".pilotrun.app") {
					t.Errorf("url = %q, want the exact label asked for", got.URL)
				}
			}
		})
	}
}

// A service that wants no public address has to be able to say so, or the
// default becomes a rule with no way out.
func TestPrivateCreateMintsNoAddress(t *testing.T) {
	h, st := labelServer(t)

	rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
		"name": "hidden", "app": "storefront", "replicas": 1, "private": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body)
	}
	got := createdService(t, rec.Body)
	if got.URL != "" {
		t.Errorf("url = %q, want none for a private service", got.URL)
	}

	row, err := st.GetService(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if row.Domain != "" {
		t.Errorf("stored domain = %q, want empty", row.Domain)
	}
}

// Two ways of saying opposite things about one field. Neither guess is safe,
// so it is a refusal rather than a precedence rule.
func TestPrivateAndDomainContradict(t *testing.T) {
	h, _ := labelServer(t)

	rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
		"name": "hidden", "replicas": 1, "private": true, "domain": "x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "contradict") {
		t.Errorf("the 400 does not say why: %s", rec.Body)
	}
}

// The create-side twin of TestPatchRefusesAServiceNothingCouldEverWake. The
// default address made this rule unreachable for an ordinary service, so this
// pins that it still fires for the one service it now describes.
func TestAPrivateServiceNothingCouldEverWakeIsRefused(t *testing.T) {
	h, _ := labelServer(t)

	rec := doJSON(t, h, "POST", "/v1/services", map[string]any{
		"name": "nowhere", "replicas": 0, "private": true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "can never be reached or woken") {
		t.Errorf("the 400 lost its message: %s", rec.Body)
	}
}

// Services that predate minted addresses keep none until their owner asks, so
// the patch is the only way they can get one. Exactly once, because changing
// an address is not expressible.
func TestPatchGivesALegacyServiceAnAddressOnce(t *testing.T) {
	h, st := labelServer(t)
	ctx := context.Background()
	seed := func(t *testing.T, id string) {
		t.Helper()
		if err := st.PutService(ctx, &state.Service{
			ID: id, Name: "legacy", Replicas: 1, App: "storefront", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
		if err := st.PutTenancy(ctx, &state.Tenancy{
			ID: id, OrgID: "org_1", Kind: "service", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("PutTenancy: %v", err)
		}
	}

	seed(t, "s_2")
	rec := doJSON(t, h, "PATCH", "/v1/services/s_2", map[string]any{"domain": "late"})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := createdService(t, rec.Body); !strings.HasSuffix(got.URL, "late.pilotrun.app") {
		t.Errorf("url = %q, want late.pilotrun.app", got.URL)
	}

	// A second address would change a URL, which is permanent.
	rec = doJSON(t, h, "PATCH", "/v1/services/s_2", map[string]any{"domain": "later"})
	if rec.Code != http.StatusConflict {
		t.Errorf("a second address got %d, want 409: %s", rec.Code, rec.Body)
	}

	// Removing one is the same rule from the other side.
	seed(t, "s_3")
	rec = doJSON(t, h, "PATCH", "/v1/services/s_3", map[string]any{"domain": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an empty address got %d, want 400: %s", rec.Code, rec.Body)
	}

	// And the namespace is checked on the patch path too.
	rec = doJSON(t, h, "PATCH", "/v1/services/s_3", map[string]any{"domain": "web"})
	if rec.Code != http.StatusConflict {
		t.Errorf("a taken address got %d, want 409: %s", rec.Code, rec.Body)
	}
}

// private is create-only: it describes what happens when the address is
// decided, and the address is decided once.
func TestPatchRefusesPrivate(t *testing.T) {
	h, _ := labelServer(t)

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{"private": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "private") {
		t.Errorf("the 400 does not name the field: %s", rec.Body)
	}
}
