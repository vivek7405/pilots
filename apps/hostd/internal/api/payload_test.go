package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// bigEnv is an environment map of roughly n bytes once encoded.
func bigEnv(n int) map[string]string {
	out := map[string]string{}
	for len(mustJSON(out)) < n {
		out["K"+strings.Repeat("x", 8)+itoa(len(out))] = strings.Repeat("v", 512)
	}
	return out
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// A row is gossiped to every host, held in each one's memory, and carried in
// every backup, for as long as the object lives. Fly retried a row too large
// to apply within corrosion's timeout forever, starving every other update on
// the host, and the row was accepted at an API with no opinion about its size.
func TestAnOversizedEnvIsRefusedOnAServiceCreate(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	body := mustJSON(map[string]any{
		"name": "big", "image": "bld-1", "env": bigEnv(maxEnvBytes + 1024),
	})
	rec := postJSON(t, h, "/v1/services", testKey, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Code != CodePayloadTooLarge {
		t.Errorf("code = %q, want %q", errResp.Code, CodePayloadTooLarge)
	}
	if !strings.Contains(errResp.Error, "env") {
		t.Errorf("the refusal does not name the field: %q", errResp.Error)
	}
}

func TestAnOversizedEnvIsRefusedOnAMachineCreate(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	body := mustJSON(map[string]any{
		"vcpus": 1, "mem_mib": 512, "env": bigEnv(maxEnvBytes + 1024),
	})
	rec := postJSON(t, h, "/v1/machines", testKey, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// An ordinary payload must be untouched. The limits are generous on purpose:
// a cap that refused real applications would be worse than the problem.
func TestAnOrdinaryEnvIsAccepted(t *testing.T) {
	if !checkPayloadSize(nil, map[string]any{
		"env": map[string]string{"NODE_ENV": "production", "PORT": "8080"},
	}) {
		t.Error("an ordinary environment was refused")
	}
	// Two hundred variables of a hundred bytes is still well inside.
	env := map[string]string{}
	for i := 0; i < 200; i++ {
		env["VAR"+itoa(i)] = strings.Repeat("x", 100)
	}
	if !checkPayloadSize(nil, map[string]any{"env": env}) {
		t.Error("200 ordinary variables were refused")
	}
}

// The policy fields have a tighter limit than env, because none of them has a
// legitimate reason to be large.
func TestAPolicyFieldHasItsOwnLimit(t *testing.T) {
	rec := newRecorder()
	labels := map[string]string{}
	for len(mustJSON(labels)) < maxPolicyBytes+512 {
		labels["l"+itoa(len(labels))] = strings.Repeat("v", 256)
	}
	if checkPayloadSize(rec, map[string]any{"labels": labels}) {
		t.Error("an oversized labels map was accepted")
	}
	if !strings.Contains(rec.Body.String(), "labels") {
		t.Errorf("the refusal does not name the field: %s", rec.Body.String())
	}
}

func TestEncodedSizeTreatsNilAsNothing(t *testing.T) {
	for _, v := range []any{nil, map[string]string(nil), []string(nil)} {
		n, err := encodedSize(v)
		if err != nil || n != 0 {
			t.Errorf("encodedSize(%v) = %d, %v; want 0", v, n, err)
		}
	}
}
