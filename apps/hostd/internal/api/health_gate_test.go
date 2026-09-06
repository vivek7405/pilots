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

// gateFailingRollout refuses every deploy the way the real rollout does when
// the replica never answers its health check.
type gateFailingRollout struct{ recordingRollout }

func (r *gateFailingRollout) Deploy(context.Context, string, string,
	json.RawMessage) (*state.Release, error) {

	return nil, &HealthGateDetails{
		Service: "svc_1", Replica: "m_9", Release: "rel_1", GraceSec: 40,
		Last: HealthLast{
			Error: "connection refused on port 8080: the app is not listening on 0.0.0.0:$PORT",
		},
	}
}

// A deploy the health gate refused is a 422 carrying the replica, not a 500
// carrying a 10.x address.
//
// It was a 500 before this: writeStoreError's default arm, with the probe URL
// formatted into the message. A 500 says the platform broke, so nobody looked
// at their app, and the address it printed is inside a network namespace on
// one host and reaches nothing from a laptop.
func TestADeployTheGateRefusedIs422WithTheReplica(t *testing.T) {
	h := gateFailingServer(t)
	rec := doJSON(t, h, "POST", "/v1/services/svc_1/deploy",
		json.RawMessage(`{"build":"b_1"}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error   string            `json:"error"`
		Code    string            `json:"code"`
		Next    string            `json:"next"`
		Details HealthGateDetails `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body: %v (%s)", err, rec.Body.String())
	}
	if got.Code != CodeHealthGateFailed {
		t.Errorf("code = %q, want %q", got.Code, CodeHealthGateFailed)
	}
	if got.Details.Replica != "m_9" {
		t.Errorf("details.replica = %q, want m_9", got.Details.Replica)
	}
	if got.Details.Service != "svc_1" || got.Details.Release != "rel_1" {
		t.Errorf("details = %+v, want the service and release filled", got.Details)
	}
	if !strings.Contains(got.Details.Last.Error, "connection refused") {
		t.Errorf("details.last.error = %q, want the refused-connection line", got.Details.Last.Error)
	}
	if got.Next == "" {
		t.Error("a 422 with no next: nothing tells the caller what to do about it")
	}
	body := rec.Body.String()
	if regexp.MustCompile(`\b10\.\d+\.\d+\.\d+\b`).MatchString(body) {
		t.Errorf("the body carries a host-internal address: %s", body)
	}
}

func gateFailingServer(t *testing.T) http.Handler {
	t.Helper()
	return deployServerWith(t, &gateFailingRollout{})
}
