package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// stop and start answered 501 while the CLI and both SDKs already called them,
// so the commonest lifecycle pair in the product was "not implemented" on a
// machine that could do it perfectly well. They are suspend and wake under the
// names every other platform uses.
func TestStopAndStartAreNoLongerUnimplemented(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := do(t, h, "POST", "/v1/machines/m_1/stop", testKey)
	if rec.Code == http.StatusNotImplemented {
		t.Fatal("POST /v1/machines/{id}/stop still answers 501")
	}
	if fake.suspended != 1 {
		t.Errorf("stop drove %d suspends, want 1", fake.suspended)
	}

	rec = do(t, h, "POST", "/v1/machines/m_1/start", testKey)
	if rec.Code == http.StatusNotImplemented {
		t.Fatal("POST /v1/machines/{id}/start still answers 501")
	}
	if fake.woken != 1 {
		t.Errorf("start drove %d wakes, want 1", fake.woken)
	}
}

func TestProcessesArePassedThroughFromTheGuest(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)
	fake.processes = `{"processes":[{"name":"web","state":"running","pid":42}]}`

	rec := do(t, h, "GET", "/v1/machines/m_1/processes", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Processes []struct {
			Name  string `json:"name"`
			State string `json:"state"`
			PID   int    `json:"pid"`
		} `json:"processes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Processes) != 1 || got.Processes[0].Name != "web" || got.Processes[0].PID != 42 {
		t.Errorf("processes = %+v, want the guest's own answer unchanged", got.Processes)
	}
}

// The property that makes processes worth naming: an action names ONE of them.
func TestAProcessActionNamesOneProcess(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := do(t, h, "POST", "/v1/machines/m_1/processes/worker/restart", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.processActions) != 1 || !strings.HasSuffix(fake.processActions[0], "restart worker") {
		t.Errorf("actions = %v, want one restart of worker", fake.processActions)
	}
}

func TestProcessLogsCarryTheTail(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := do(t, h, "GET", "/v1/machines/m_1/processes/web/logs?tail=25", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if fake.processLogTail != 25 {
		t.Errorf("tail reached the manager as %d, want 25", fake.processLogTail)
	}
	if !strings.Contains(rec.Body.String(), "logs of web") {
		t.Errorf("body = %q, want the named process's output", rec.Body.String())
	}
}
