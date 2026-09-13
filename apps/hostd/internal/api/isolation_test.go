package api

import (
	"strings"
	"testing"
)

// A machine's own token may not read another service's secrets.
//
// # The bug
//
// selfAllows exempted every read from the self-only narrowing, on the stated
// premise that a machine which can list its siblings can do nothing with that
// alone. The same change that wrote that premise added
// GET /v1/services/{id}/env, which calls FleetKey.Open and answers with the
// DECRYPTED values. So one sandbox's broker token read every service's
// credentials in its org.
//
// The credential broker exists so nothing lands in the guest. This handed the
// guest a route that returns the secrets directly.
func TestAMachinesTokenCannotReadAnotherServicesSecrets(t *testing.T) {
	r := asSelf(request("GET", "/v1/services/svc-billing/env"), "m-1", "svc-mine")

	if selfAllows(r, "svc-billing", "svc-billing") {
		t.Fatal("a machine's token was allowed to read a sibling service's " +
			"decrypted env; the broker exists so secrets never reach a guest")
	}
	// Its OWN service is still readable, or a machine cannot find its config.
	own := asSelf(request("GET", "/v1/services/svc-mine/env"), "m-1", "svc-mine")
	if !selfAllows(own, "svc-mine", "svc-mine") {
		t.Error("a machine was refused its own service's env")
	}
}

// An ordinary metadata read stays org-wide, or the narrowing has broken the
// reason a machine holds a token at all.
func TestAMachinesTokenStillReadsSiblingMetadata(t *testing.T) {
	r := asSelf(request("GET", "/v1/machines/m-2"), "m-1", "")

	if !selfAllows(r, "m-2", "") {
		t.Fatal("a machine may no longer read a sibling's row; reads stay " +
			"org-wide because a read of metadata does nothing on its own")
	}
}

// The volume chokepoint narrows a self token like the other two.
//
// self.go argues the check belongs where ownership is resolved so no handler
// can be forgotten. There are three resolvers and this one was missed, so a
// broker token could restore a sibling service's database to an old snapshot,
// delete every snapshot of it, or overwrite its backup schedule.
func TestAMachinesTokenCannotActOnASiblingsVolume(t *testing.T) {
	for _, path := range []string{
		"/v1/volumes/vol-sibling/snapshots/2026-01-01T00:00:00Z/restore",
		"/v1/volumes/vol-sibling/snapshots/2026-01-01T00:00:00Z",
		"/v1/volumes/vol-sibling/policy",
	} {
		r := asSelf(request("POST", path), "m-1", "")
		if selfAllows(r, "vol-sibling", "") {
			t.Errorf("a machine's token was allowed to act on %s", path)
		}
	}
}

// An ordinary key is untouched by any of it: Self is empty for every one.
func TestAnOrdinaryKeyIsNotNarrowed(t *testing.T) {
	r := request("GET", "/v1/services/svc-any/env")
	if !selfAllows(r, "svc-any", "") {
		t.Fatal("an ordinary API key was narrowed as though it were a machine's")
	}
}

// The two tenant-facing routes added by this branch claim a scope.
//
// scopeAllows fails closed to admin for a path no row matches, so a missing
// row is not a smaller permission, it is a 403 for every key `pilot login`
// mints. `pilot add postgres` fetches its recipe over one of these.
func TestTheTenantRoutesAreReachableByATenantKey(t *testing.T) {
	for _, tc := range []struct{ path, scopes string }{
		{"/v1/recipes/postgres", "deploy"},
		{"/v1/recipes/ha/db", "deploy"},
		{"/v1/egress", "machines"},
		{"/v1/egress", "deploy"},
	} {
		need, ok := scopeAllows(tc.scopes, tc.path)
		if !ok {
			t.Errorf("%s needs scope %q, which a %q key does not carry; the "+
				"route is unreachable by the callers it was written for",
				tc.path, need, tc.scopes)
		}
	}
}

// And a path nobody claimed still fails closed, which is what makes the
// missing rows above a bug rather than a style question.
func TestAnUnclaimedPathStillNeedsAdmin(t *testing.T) {
	need, ok := scopeAllows("deploy", "/v1/something-nobody-added")
	if ok || !strings.Contains(string(need), "admin") {
		t.Fatalf("an unclaimed path resolved to %q/%v; it must fail closed", need, ok)
	}
}
