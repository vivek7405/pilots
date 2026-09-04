package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// recordingRollout answers a deploy and remembers the knobs it was handed, so
// a test can tell "refused" from "accepted and quietly dropped".
type recordingRollout struct {
	deploys   int
	lastKnobs json.RawMessage
}

func (r *recordingRollout) Deploy(_ context.Context, serviceID, _ string,
	knobs json.RawMessage) (*state.Release, error) {

	r.deploys++
	r.lastKnobs = knobs
	return &state.Release{ID: "rel_1", ServiceID: serviceID}, nil
}

func (r *recordingRollout) Rollback(context.Context, string) (*state.Release, error) {
	return &state.Release{ID: "rel_1"}, nil
}

func (r *recordingRollout) Promote(context.Context, string, PromoteRequest) (*state.Service, error) {
	return &state.Service{ID: "svc_1"}, nil
}

// deployServer is a host that can actually deploy: a service to deploy to, a
// build to deploy, and a rollout to hand them to.
func deployServer(t *testing.T) (http.Handler, *recordingRollout) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	sum := sha256.Sum256([]byte(testKey))
	if err := st.PutAPIKey(ctx, &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_1", Scopes: "admin",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	if err := st.PutService(ctx, &state.Service{
		ID: "svc_1", Name: "web", Replicas: 1, ReleaseID: "rel_0",
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{
		ID: "svc_1", OrgID: "org_1", Kind: "service",
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}

	roll := &recordingRollout{}
	return Routes(Deps{HostID: "host-test", Store: st, Machines: newFakeManager(),
		Rollout: roll}), roll
}

// A deploy's knobs reach the rollout, which merges them PARTIALLY onto what
// the previous release's replicas carry and discards its own decode error --
// it has no way to report a bad field without also discarding the good ones.
// So the refusal has to happen here, where the whole body can still be
// rejected.
//
// Unvalidated, {"min_machines_running":"one"} was a 200 whose replicas
// silently kept the inherited floor: an operator asking for a warm replica,
// told it worked, and given one that still sleeps.
func TestADeployWithMalformedKnobsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		knobs string
	}{
		{"a field of the wrong type", `{"min_machines_running":"one"}`},
		{"knobs that are not an object", `3`},
		{"truncated json", `{"auto_stop":`},
		// encoding/json applies what it can read BEFORE returning the type
		// error, so this one is worse than all-or-nothing: unrefused, the
		// floor lands and soft_limit does not. A half-applied policy.
		{"one good field and one bad", `{"soft_limit":"x","min_machines_running":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, roll := deployServer(t)
			rec := doJSON(t, h, "POST", "/v1/services/svc_1/deploy",
				json.RawMessage(`{"build":"b_1","knobs":`+tc.knobs+`}`))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if roll.deploys != 0 {
				t.Error("the rollout ran anyway, so replicas were made from knobs nobody could read")
			}
		})
	}
}

// The other half: a partial policy is the normal case and must still deploy,
// reaching the rollout with the caller's fields and nothing else, so the
// merge onto the sibling replicas' policy still has something to merge.
func TestAPartialKnobsObjectStillDeploys(t *testing.T) {
	h, roll := deployServer(t)
	rec := doJSON(t, h, "POST", "/v1/services/svc_1/deploy",
		json.RawMessage(`{"build":"b_1","knobs":{"min_machines_running":1}}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if want := `{"min_machines_running":1}`; string(roll.lastKnobs) != want {
		t.Errorf("the rollout was handed %s, want %s -- validating must not "+
			"rewrite the body into a full policy, or the merge zeroes the rest",
			roll.lastKnobs, want)
	}
}

// A deploy with no knobs at all is the common path and says nothing about
// policy, so it must not be turned into an empty one.
func TestADeployWithNoKnobsIsUnaffected(t *testing.T) {
	h, roll := deployServer(t)
	rec := doJSON(t, h, "POST", "/v1/services/svc_1/deploy",
		json.RawMessage(`{"build":"b_1"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(roll.lastKnobs) != 0 {
		t.Errorf("the rollout was handed %s, want nothing", roll.lastKnobs)
	}
}
