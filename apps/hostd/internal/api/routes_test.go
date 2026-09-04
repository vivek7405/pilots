package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

const testKey = "pilot_testkey"

func newTestServer(t *testing.T) (http.Handler, state.Store) {
	h, st, _ := newTestServerWithManager(t)
	return h, st
}

func newTestServerWithManager(t *testing.T) (http.Handler, state.Store, *fakeManager) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// admin, because this helper's server is used by every test in the
	// package and the battery of routes spans all three scopes. Narrower
	// scopes get their own tests in auth_test.go, where the refusal is the
	// assertion rather than an accident.
	sum := sha256.Sum256([]byte(testKey))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_1", Scopes: "admin",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	fake := newFakeManager()
	// Seed a machine so the read paths have something to return.
	if err := st.PutMachine(context.Background(), fake.machine); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	return Routes(Deps{HostID: "host-test", Store: st, Machines: fake}), st, fake
}

func do(t *testing.T, h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthIsPublic(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/health: got %d, want 200", rec.Code)
	}
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.HostID != "host-test" {
		t.Errorf("health payload = %+v", got)
	}
	// SQLite: no replica, so the field is a real 0 rather than absent. The
	// gate reads it as a number on every host, single-box ones included.
	if got.StoreVersion != 0 {
		t.Errorf("store_version = %d with no replica, want 0", got.StoreVersion)
	}
}

// A host that has fallen behind on replication answers 200 and looks healthy
// from every other angle. store_version is the only thing on this response
// that says otherwise, which is why it is on the unauthenticated health route
// rather than behind an admin key.
func TestHealthReportsTheStoreVersion(t *testing.T) {
	h := Routes(Deps{
		HostID:       "host-test",
		StoreVersion: func(context.Context) (int64, error) { return 42, nil },
	})
	rec := do(t, h, "GET", "/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StoreVersion != 42 {
		t.Errorf("store_version = %d, want 42", got.StoreVersion)
	}
}

// A store that cannot be read is not a dead host. Failing liveness on a
// replica hiccup takes the host out of rotation for a problem that is not
// the host's, and every machine it owns with it.
func TestHealthStaysOKWhenTheStoreVersionCannotBeRead(t *testing.T) {
	h := Routes(Deps{
		HostID: "host-test",
		StoreVersion: func(context.Context) (int64, error) {
			return 0, errors.New("corrosion: connection refused")
		},
	})
	rec := do(t, h, "GET", "/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 even with the store unreadable", rec.Code)
	}
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.StoreVersion != 0 {
		t.Errorf("health payload = %+v, want ok with store_version 0", got)
	}
}

// A wedged replica is the case the error case above does not cover: the
// corrosion client sets no response timeout, so an agent that accepts the
// connection and then never answers would hold this handler open for as long
// as the caller waits. On the one unauthenticated route every load balancer
// polls, that is how a replica hiccup becomes the host being taken out of
// rotation -- and it is cheap to trigger, since the route needs no key.
//
// The handler must give up on its own and answer 200 with 0.
func TestHealthDoesNotWaitOnAWedgedStore(t *testing.T) {
	released := make(chan struct{})
	defer close(released)

	h := Routes(Deps{
		HostID: "host-test",
		StoreVersion: func(ctx context.Context) (int64, error) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-released:
				return 99, nil
			}
		},
	})

	start := time.Now()
	rec := do(t, h, "GET", "/v1/health", "")
	took := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 with the store wedged", rec.Code)
	}
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.StoreVersion != 0 {
		t.Errorf("health payload = %+v, want ok with store_version 0", got)
	}
	// Generous, so the test is not a stopwatch: what it rules out is the
	// handler waiting on the store indefinitely.
	if took > 5*storeVersionTimeout {
		t.Errorf("health took %s, want it bounded by %s", took, storeVersionTimeout)
	}
}

func TestMetricsIsPublic(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The host families render even on a host that has done nothing, which is
	// what makes a scrape of a fresh host readable rather than empty.
	for _, name := range []string{
		"pilots_wake_seconds",
		"pilots_checkpoint_durable_seconds",
		"pilots_nbd_cache_hits_total",
		"pilots_nbd_cache_misses_total",
		"pilots_router_inflight",
		"pilots_slots_free",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("%s is missing from the scrape", name)
		}
	}

	// Cardinality is the one thing that cannot be fixed after the fact: a
	// series per machine melts the scrape exactly when a host is busiest.
	for _, label := range []string{"machine_id=", "org_id=", "host_id="} {
		if strings.Contains(body, label) {
			t.Errorf("the scrape carries a %s label", label)
		}
	}
}

func TestUnauthenticatedIsRejected(t *testing.T) {
	h, _ := newTestServer(t)
	for _, tc := range []struct{ name, key string }{
		{"no header", ""},
		{"wrong key", "pilot_wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(t, h, "GET", "/v1/machines", tc.key); rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", rec.Code)
			}
		})
	}
}

func TestMalformedAuthHeaderIsRejected(t *testing.T) {
	h, _ := newTestServer(t)
	for _, hdr := range []string{"Basic abc", "Bearer", "Bearer ", testKey} {
		req := httptest.NewRequest("GET", "/v1/machines", nil)
		req.Header.Set("Authorization", hdr)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: got %d, want 401", hdr, rec.Code)
		}
	}
}

// Every route in the table must exist. A 404 here means a typo in the pattern,
// which is exactly what this test is for -- the CLI and both SDKs are written
// against this list before the handlers exist.
func TestEveryRouteIsRegistered(t *testing.T) {
	h, _ := newTestServer(t)

	routes := []struct{ method, path string }{
		{"POST", "/v1/machines"},
		{"GET", "/v1/machines"},
		{"GET", "/v1/machines/m_1"},
		{"DELETE", "/v1/machines/m_1"},
		{"POST", "/v1/machines/m_1/exec"},
		{"GET", "/v1/machines/m_1/exec/stream"},
		{"GET", "/v1/sprites/webapp/exec"},
		{"GET", "/v1/machines/m_1/logs"},
		{"POST", "/v1/machines/m_1/suspend"},
		{"POST", "/v1/machines/m_1/wake"},
		{"POST", "/v1/machines/m_1/stop"},
		{"POST", "/v1/machines/m_1/start"},
		{"POST", "/v1/machines/m_1/checkpoints"},
		{"GET", "/v1/machines/m_1/checkpoints"},
		{"POST", "/v1/checkpoints/c_1/restore"},
		{"POST", "/v1/builds"},
		{"POST", "/v1/services"},
		{"GET", "/v1/services"},
		{"GET", "/v1/services/s_1"},
		{"POST", "/v1/services/s_1/deploy"},
		{"POST", "/v1/services/s_1/rollback"},
		{"POST", "/v1/machines/m_1/promote"},
		{"POST", "/v1/volumes"},
		{"GET", "/v1/volumes"},
		{"GET", "/v1/hosts"},
		{"POST", "/v1/api-keys"},
		{"GET", "/v1/api-keys"},
		{"POST", "/v1/api-keys/abc/revoke"},
		{"GET", "/v1/quotas/org_1"},
		{"PUT", "/v1/quotas/org_1"},
	}

	for _, r := range routes {
		rec := do(t, h, r.method, r.path, testKey)
		// A 404 is ambiguous now that these handlers are real: an unregistered
		// pattern and a registered handler that could not find the row both
		// answer 404. Go's mux writes a bare "404 page not found" for the
		// former, while a handler answers the JSON error shape -- so the body
		// is what distinguishes them.
		if rec.Code == http.StatusNotFound && !strings.Contains(rec.Body.String(), "{") {
			t.Errorf("%s %s: 404 with no JSON body -- route not registered", r.method, r.path)
		}
		// Anything that is not a 404 proves the pattern matched. Handlers that
		// are still unimplemented answer 501; implemented ones answer for real.
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s: 405 -- pattern registered under the wrong method", r.method, r.path)
		}
	}
}

// Auth must be enforced before the 501, or an unauthenticated caller could map
// the entire API surface.
func TestAuthPrecedesHandler(t *testing.T) {
	h, _ := newTestServer(t)
	if rec := do(t, h, "POST", "/v1/machines/m_1/promote", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401 (auth must run before the handler)", rec.Code)
	}
}
