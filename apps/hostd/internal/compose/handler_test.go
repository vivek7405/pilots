package compose

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/compose/plan", strings.NewReader(body))
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandlerAnswersAPlan(t *testing.T) {
	body, _ := json.Marshal(Request{
		Compose: fixture,
		Env:     map[string]string{"COMPOSE_PROJECT_NAME": "shop"},
	})
	rec := post(t, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got Plan
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.App != "shop" || len(got.Steps) != 3 {
		t.Fatalf("plan = %+v", got)
	}
	if strings.Join(names(&got), ",") != "postgres,web,worker" {
		t.Errorf("steps = %v", names(&got))
	}
}

func TestHandlerAnswersAPlanErrorWithEveryKey(t *testing.T) {
	body, _ := json.Marshal(Request{Compose: `
name: shop
services:
  web:
    image: nginx
    privileged: true
`})
	rec := post(t, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
	var got PlanError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != unsupportedError {
		t.Errorf("error = %q", got.Error)
	}
	if len(got.Unsupported) != 1 || got.Unsupported[0].Key != "privileged" {
		t.Errorf("unsupported = %+v", got.Unsupported)
	}
}

// Every way a caller can get this wrong is a 400: the file is theirs, and all
// of it is fixable.
func TestHandlerRefusesWhatItCannotPlan(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not json", "{", "unexpected"},
		{"no compose", `{"compose":""}`, "compose is required"},
		{"a cycle", `{"compose":"name: s\nservices:\n  a:\n    image: n\n    depends_on: [b]\n  b:\n    image: n\n    depends_on: [a]\n"}`, "cycle"},
		{"an unset variable", `{"compose":"name: s\nservices:\n  a:\n    image: ${TAG}\n"}`, "TAG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body %s does not carry %q", rec.Body, tc.want)
			}
		})
	}
}

// The same 1 MiB cap the rest of the API decodes under. A body past it is cut
// mid-JSON, so it fails to decode -- which is a 400 naming the parse, not a
// hang and not a host reading an arbitrarily large file into memory.
func TestHandlerCapsTheBody(t *testing.T) {
	huge := `{"compose":"name: shop\nservices:\n  web:\n    image: nginx\n# ` +
		strings.Repeat("x", 2<<20) + `"}`
	rec := post(t, huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a 2 MiB body got %d, want 400", rec.Code)
	}
}
