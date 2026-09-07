package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// dependsServer seeds an app the canvas would draw: web, db and cache in app
// "shop" owned by org_1, plus a service of another app and one of another org
// carrying the same names, which is what the two halves of the sibling key are
// there to keep apart.
//
// The seeded web row carries whatever env and sealed env the caller gives, so
// each case below differs by exactly the environment under test.
func dependsServer(t *testing.T, key Sealer, env, sealed string) http.Handler {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")

	ctx := context.Background()
	rows := []struct {
		id, name, app, org, env, sealed string
	}{
		{"s_web", "web", "shop", "org_1", env, sealed},
		{"s_db", "db", "shop", "org_1", "", ""},
		{"s_cache", "cache", "shop", "org_1", "", ""},
		// Same name, another app of the same org: not a sibling.
		{"s_other", "queue", "warehouse", "org_1", "", ""},
		// Same app name, another org: not a sibling either.
		{"s_foreign", "billing", "shop", "org_2", "", ""},
	}
	for _, r := range rows {
		if err := st.PutService(ctx, &state.Service{
			ID: r.id, Name: r.name, App: r.app, Replicas: 1, Domain: r.name,
			Env: r.env, EnvSealed: r.sealed, CreatedAt: 1,
		}); err != nil {
			t.Fatalf("PutService %s: %v", r.id, err)
		}
		if err := st.PutTenancy(ctx, &state.Tenancy{
			ID: r.id, OrgID: r.org, Kind: "service", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("PutTenancy %s: %v", r.id, err)
		}
	}
	return Routes(Deps{
		HostID: "host-test", Store: st, Domain: "pilotrun.app", FleetKey: key,
	})
}

// readWeb reads the seeded web service and returns the decoded body next to
// its raw text, so a case can assert on the edges and on what the body must
// never contain in one place.
func readWeb(t *testing.T, h http.Handler, path string) (Service, string) {
	t.Helper()
	rec := do(t, h, "GET", path, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200 (%s)", path, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.HasPrefix(strings.TrimSpace(body), "[") {
		var rows []Service
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, row := range rows {
			if row.ID == "s_web" {
				return row, body
			}
		}
		t.Fatalf("the list does not carry s_web: %s", body)
	}
	var out Service
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, body
}

func wantEdges(t *testing.T, got Service, want []string) {
	t.Helper()
	if len(want) == 0 {
		if len(got.DependsOn) != 0 {
			t.Errorf("depends_on = %v, want none", got.DependsOn)
		}
		return
	}
	if !reflect.DeepEqual(got.DependsOn, want) {
		t.Errorf("depends_on = %v, want %v", got.DependsOn, want)
	}
}

// The plain half alone is enough for a service that dials a sibling over a
// non-secret address.
func TestAPlainInternalReferenceIsAnEdge(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"UPSTREAM":"http://db.internal:5432"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, []string{"db"})
}

// The sealed half is where a real dependency lives: a database URL is a
// secret, so a canvas drawn from the plaintext half alone would be empty for
// exactly the app this exists for.
func TestASealedInternalReferenceIsAnEdge(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, "",
		`sealed:{"DATABASE_URL":"postgres://u:pw@db.internal:5432/app"}`)
	got, body := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, []string{"db"})
	if strings.Contains(body, "postgres://") || strings.Contains(body, "pw@") {
		t.Errorf("the body carries the decrypted value: %s", body)
	}
}

// Both halves at once, and the answer is sorted, so a canvas does not reorder
// itself between two reads of the same service.
func TestBothHalvesAreReadAndTheAnswerIsSorted(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true},
		`{"CACHE":"redis://cache.internal:6379"}`,
		`sealed:{"DATABASE_URL":"postgres://u:pw@db.internal:5432/app"}`)
	got, body := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, []string{"cache", "db"})
	if strings.Contains(body, "postgres://") || strings.Contains(body, "redis://") {
		t.Errorf("the body carries an environment value: %s", body)
	}
}

// A suffix is not a name: mydb.internal is its own hostname and draws no edge
// to db. The greedy capture is what makes this hold, so it has no one-line
// counterfactual, but it is the case a reader will worry about and it is
// pinned here.
func TestASuffixIsNotAnEdge(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"UPSTREAM":"http://mydb.internal:5432"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, nil)
}

// Nor is a longer suffix on the other side. Without the trailing character
// class, db.internalfoo would draw an edge to db, and that is a hostname
// nothing on this fleet resolves.
func TestATrailingSuffixIsNotAnEdge(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"UPSTREAM":"http://db.internalfoo/x"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, nil)
}

// A service that reads its own address does not depend on itself: a self-edge
// is a cycle the layout would have to break for no reason.
func TestAServiceDoesNotDependOnItself(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"SELF":"http://web.internal:3000"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, nil)
}

// Another app of the same org is not a sibling: <name>.internal resolves
// within an app, so an edge across two of them would be an edge nothing dials.
func TestAnotherAppIsNotASibling(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"Q":"http://queue.internal:4000"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, nil)
}

// Another org's service is not a sibling even when the app names match, or one
// tenant's canvas would name another tenant's service.
func TestAnotherOrgIsNotASibling(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"B":"http://billing.internal:4000"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, nil)
}

// A name that is not a service of this app is dropped rather than drawn: an
// edge to a card that does not exist is worse than no edge.
func TestAnUnknownNameIsNotAnEdge(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"X":"http://elsewhere.internal:80"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, nil)
}

// A host with no fleet key still answers, from the plaintext half. The sealed
// blob it cannot open is skipped, not fatal: the service is the answer and the
// edges are a drawing.
func TestAHostWithNoKeyDerivesFromThePlaintextHalf(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: false},
		`{"CACHE":"redis://cache.internal:6379"}`,
		`sealed:{"DATABASE_URL":"postgres://u:pw@db.internal:5432/app"}`)
	got, body := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, []string{"cache"})
	if strings.Contains(body, "postgres://") {
		t.Errorf("the body carries the sealed value: %s", body)
	}
}

// The list endpoint derives the same edges the single read does. It is the one
// the canvas actually calls, and it takes a different path through the
// derivation: one grouping pass rather than one list per row.
func TestTheListDerivesTheSameEdges(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true},
		`{"CACHE":"redis://cache.internal:6379"}`,
		`sealed:{"DATABASE_URL":"postgres://u:pw@db.internal:5432/app"}`)
	got, body := readWeb(t, h, "/v1/services")
	wantEdges(t, got, []string{"cache", "db"})
	if strings.Contains(body, "postgres://") || strings.Contains(body, "redis://") {
		t.Errorf("the list carries an environment value: %s", body)
	}
	// A service nothing dials carries no edges at all, so the field is absent
	// rather than an empty array.
	if strings.Contains(body, `"depends_on":[]`) {
		t.Errorf("an empty depends_on was rendered: %s", body)
	}
}

// Two references separated by exactly ONE character. The pattern consumes the
// delimiter on both sides of a match and RE2 has no lookahead, so a scan that
// resumed after the whole match had no delimiter left for the second
// reference and silently returned only the first. A comma-separated host list
// is ordinary in an environment value, and half its arrows went missing.
//
// Counterfactual: put FindAllStringSubmatch back in internalNames and every
// case here that uses a single separator fails, while the two-separator cases
// above keep passing, which is why nothing caught it.
func TestTwoReferencesSeparatedByOneCharacterAreBothEdges(t *testing.T) {
	for _, env := range []string{
		`{"HOSTS":"db.internal,cache.internal"}`,
		`{"HOSTS":"db.internal cache.internal"}`,
		`{"HOSTS":"db.internal;cache.internal"}`,
	} {
		h := dependsServer(t, fakeSealer{set: true}, env, "")
		got, _ := readWeb(t, h, "/v1/services/s_web")
		wantEdges(t, got, []string{"cache", "db"})
	}
}

// The same, with a separator on each side, which always worked. Kept so the
// fix cannot be "special-case a comma".
func TestTwoReferencesWithPortsAreBothEdges(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"HOSTS":"db.internal:5432,cache.internal:6379"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, []string{"cache", "db"})
}

// A suffix immediately after a real reference is still not an edge: the
// resumed scan must not have widened what counts as a name.
func TestAResumedScanStillRejectsASuffix(t *testing.T) {
	h := dependsServer(t, fakeSealer{set: true}, `{"HOSTS":"db.internal,cache.internalfoo"}`, "")
	got, _ := readWeb(t, h, "/v1/services/s_web")
	wantEdges(t, got, []string{"db"})
}
