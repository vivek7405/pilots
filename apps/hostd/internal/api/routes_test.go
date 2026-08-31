package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

const testKey = "pilot_testkey"

func newTestServer(t *testing.T) (http.Handler, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sum := sha256.Sum256([]byte(testKey))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_1", Scopes: "machines",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	return Routes(Deps{HostID: "host-test", Store: st}), st
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
}

func TestMetricsIsPublic(t *testing.T) {
	h, _ := newTestServer(t)
	if rec := do(t, h, "GET", "/metrics", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /metrics: got %d, want 200", rec.Code)
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
	}

	for _, r := range routes {
		rec := do(t, h, r.method, r.path, testKey)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s: 404 -- route not registered", r.method, r.path)
			continue
		}
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: got %d, want 501", r.method, r.path, rec.Code)
		}
	}
}

// Auth must be enforced before the 501, or an unauthenticated caller could map
// the entire API surface.
func TestAuthPrecedesNotImplemented(t *testing.T) {
	h, _ := newTestServer(t)
	if rec := do(t, h, "POST", "/v1/machines/m_1/promote", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401 (auth must run before the handler)", rec.Code)
	}
}
