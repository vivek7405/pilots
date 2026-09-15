package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// What a broker token may reach, asserted on the RULE rather than on a list of
// handlers.
//
// The rule is what the guard is: a machine's own token may write to its own
// machine and its own service, and nothing else, and a GET that opens a shell
// or a raw tunnel counts as a write. Testing the rule catches the handler
// somebody adds next year, which a per-handler test cannot.

func request(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func asSelf(r *http.Request, machine, service string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey, principal{
		OrgID: "org_1", Scopes: []string{ScopeMachines},
		Self: machine, SelfService: service,
	}))
}

func TestAnOrdinaryKeyIsNeverNarrowed(t *testing.T) {
	// Self empty is every API key there has ever been. If this ever returns
	// false, the guard has started refusing ordinary operators.
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		r := request(method, "/v1/machines/m_2/suspend")
		if !selfAllows(r, "m_2", "s_2") {
			t.Errorf("%s refused for a principal with no Self", method)
		}
	}
}

func TestAMachinesTokenMayWriteToItselfAndItsOwnService(t *testing.T) {
	r := asSelf(request("POST", "/v1/machines/m_1/suspend"), "m_1", "s_1")
	if !selfAllows(r, "m_1", "s_1") {
		t.Error("a machine may not act on itself")
	}
	r = asSelf(request("POST", "/v1/services/s_1/deploy"), "m_1", "s_1")
	if !selfAllows(r, "s_1", "s_1") {
		t.Error("a replica may not act on the service it belongs to")
	}
}

// The one this whole guard exists for. A compromised replica restarting its
// peers one by one is an outage it can cause on its own, so a machine may not
// write to a sibling even inside its own service.
func TestAMachinesTokenMayNotWriteToASibling(t *testing.T) {
	r := asSelf(request("POST", "/v1/machines/m_2/destroy"), "m_1", "s_1")
	if selfAllows(r, "m_2", "s_1") {
		t.Error("a machine may destroy a sibling in its own service")
	}
	r = asSelf(request("POST", "/v1/services/s_2/deploy"), "m_1", "s_1")
	if selfAllows(r, "s_2", "s_2") {
		t.Error("a replica may deploy another service")
	}
}

// A shell is not a read. Both of these are GET because they are WebSocket
// upgrades, and a rule that looked only at the method would hand a machine a
// shell on every sibling it can see.
func TestAShellAndATunnelCountAsWritesEvenThoughTheyAreGETs(t *testing.T) {
	for _, path := range []string{
		"/v1/machines/m_2/exec/stream",
		"/v1/machines/m_2/tcp/5432",
		"/v1/machines/m_2/attach",
		"/v1/machines/m_2/console",
	} {
		r := asSelf(request("GET", path), "m_1", "s_1")
		if selfAllows(r, "m_2", "s_1") {
			t.Errorf("%s was treated as a read, so a machine can open it on a sibling", path)
		}
	}
}

// Reads stay org-wide on purpose: a machine that can list its siblings can do
// nothing with that alone, and narrowing them would break the ordinary reason a
// machine holds a token.
func TestAMachinesTokenStillReadsAcrossItsOrg(t *testing.T) {
	for _, path := range []string{
		"/v1/machines/m_2",
		"/v1/services/s_2",
		"/v1/machines/m_2/logs",
	} {
		r := asSelf(request("GET", path), "m_1", "s_1")
		if !selfAllows(r, "m_2", "s_2") {
			t.Errorf("%s was refused; reads are deliberately org-wide", path)
		}
	}
}

// A sandbox has no service, and must not inherit one by the empty string
// matching an object whose service id is also empty.
func TestAMachineWithNoServiceDoesNotMatchEveryServicelessObject(t *testing.T) {
	r := asSelf(request("POST", "/v1/machines/m_2/destroy"), "m_1", "")
	if selfAllows(r, "m_2", "") {
		t.Error("a sandbox's token reached another sandbox by both having no service")
	}
}
