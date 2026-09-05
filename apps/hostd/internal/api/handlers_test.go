package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	} else {
		payload = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateMachineReturnsTheMachine(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := doJSON(t, h, "POST", "/v1/machines", CreateMachineRequest{VCPUs: 2, MemMiB: 1024})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if fake.created != 1 {
		t.Errorf("Create called %d times, want 1", fake.created)
	}

	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "m_1" {
		t.Errorf("id = %q", got.ID)
	}
	// The URL is derived from the domain, so a client never has to build it.
	if got.URL != "https://webapp.pilotrun.app" {
		t.Errorf("url = %q, want it derived from the machine's domain", got.URL)
	}
}

// An empty body is a valid create: every field has a sensible default, and
// requiring a payload just to accept them would be noise.
func TestCreateMachineAcceptsEmptyBody(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest("POST", "/v1/machines", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("got %d, want 201: %s", rec.Code, rec.Body)
	}
}

func TestGetMachineReadsFromStore(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	rec := doJSON(t, h, "GET", "/v1/machines/m_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "webapp" {
		t.Errorf("name = %q", got.Name)
	}
}

// A machine that does not exist must be a 404, not a 500: a client has to be
// able to tell "gone" from "broken".
func TestGetMissingMachineIs404(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	if rec := doJSON(t, h, "GET", "/v1/machines/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
}

func TestListMachines(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	rec := doJSON(t, h, "GET", "/v1/machines", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var got []Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d machines, want 1", len(got))
	}
}

func TestLifecycleVerbsReturnNoContent(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	for _, tc := range []struct {
		path  string
		count *int
	}{
		{"/v1/machines/m_1/suspend", &fake.suspended},
		{"/v1/machines/m_1/wake", &fake.woken},
	} {
		rec := doJSON(t, h, "POST", tc.path, nil)
		if rec.Code != http.StatusNoContent {
			t.Errorf("POST %s: got %d, want 204", tc.path, rec.Code)
		}
		if *tc.count != 1 {
			t.Errorf("POST %s did not reach the manager", tc.path)
		}
	}

	if rec := doJSON(t, h, "DELETE", "/v1/machines/m_1", nil); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 204", rec.Code)
	}
	if fake.destroyed != 1 {
		t.Error("DELETE did not reach the manager")
	}
}

func TestExecRequiresACommand(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	if rec := doJSON(t, h, "POST", "/v1/machines/m_1/exec",
		ExecRequest{Cmd: ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty cmd: got %d, want 400", rec.Code)
	}

	rec := doJSON(t, h, "POST", "/v1/machines/m_1/exec", ExecRequest{Cmd: "echo hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got ExecResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Stdout != "hello\n" || got.ExitCode != 0 {
		t.Errorf("unexpected exec response: %+v", got)
	}
}

func TestCheckpointRoutes(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := doJSON(t, h, "POST", "/v1/machines/m_1/checkpoints", CheckpointRequest{Comment: "v1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create checkpoint: got %d: %s", rec.Code, rec.Body)
	}
	var ck Checkpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &ck); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ck.ID != "ck_1" || ck.Seq != 1 {
		t.Errorf("unexpected checkpoint: %+v", ck)
	}

	if rec := doJSON(t, h, "GET", "/v1/machines/m_1/checkpoints", nil); rec.Code != http.StatusOK {
		t.Errorf("list checkpoints: got %d", rec.Code)
	}

	// A restore returns the machine, because the point is that the SAME
	// machine travelled back in time rather than a new one being created.
	rec = doJSON(t, h, "POST", "/v1/checkpoints/ck_1/restore", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: got %d: %s", rec.Code, rec.Body)
	}
	var m Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.ID != "m_1" {
		t.Errorf("restore returned machine %q, want the original m_1", m.ID)
	}
	if fake.restored != 1 {
		t.Error("restore did not reach the manager")
	}
}

func TestLogsReturnPlainText(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	rec := doJSON(t, h, "GET", "/v1/machines/m_1/logs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if rec.Body.String() != "boot log" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// The URL a client is told has to be one it can open. On a host without TLS
// -- the local single box, and the three-node rig -- that means the scheme and
// the port the plain listener is actually bound to.
func TestCreateMachineURLOnPlainHTTP(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")

	fake := newFakeManager()
	if err := st.PutMachine(context.Background(), fake.machine); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	h := Routes(Deps{
		HostID: "host-test", Store: st, Machines: fake,
		URL: PublicURLFor(false, ":8080"),
	})

	rec := doJSON(t, h, "POST", "/v1/machines", CreateMachineRequest{VCPUs: 2, MemMiB: 1024})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URL != "http://webapp.pilotrun.app:8080" {
		t.Errorf("url = %q, want the listener's scheme and port", got.URL)
	}
}
