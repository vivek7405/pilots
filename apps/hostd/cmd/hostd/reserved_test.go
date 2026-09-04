package main

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The create-time reservation only guards NEW names. A machine created before
// the control API was pointed at its hostname keeps the row and the domain,
// and every request for that URL is answered by the control API from that
// deploy on -- a permanent URL that quietly stops being served, which is what
// invariant 4 exists to prevent.
//
// The counterfactual is the whole point of the table: the near-misses must NOT
// be reported, because a check that fires on machines whose URLs are fine
// trains an operator to ignore it.
func TestAPIHostnameShadowedFindsOnlyTheMachineThatLostItsURL(t *testing.T) {
	machines := []state.Machine{
		{ID: "m-shadowed", Name: "api", Domain: "api.pilotrun.app", HostID: "host-a"},
		// Hostnames are case-insensitive, so a row written with any casing is
		// the same URL to dispatch.
		{ID: "m-upper", Name: "API", Domain: "API.pilotrun.app", HostID: "host-b"},
		// dispatch compares Host before it looks at anything else, so a custom
		// domain aimed at the API hostname is swallowed the same way.
		{ID: "m-custom", Name: "beta", Domain: "beta.pilotrun.app",
			CustomDomain: "api.pilotrun.app", HostID: "host-c"},

		// Not shadowed: destroyed, so it has no URL left to lose.
		{ID: "m-gone", Name: "api", Domain: "api.pilotrun.app",
			HostID: "host-a", State: state.StateDestroyed},
		// Not shadowed: an ordinary machine.
		{ID: "m-alpha", Name: "alpha", Domain: "alpha.pilotrun.app"},
		// Not shadowed: shares a prefix with the API hostname and nothing else.
		{ID: "m-apiary", Name: "apiary", Domain: "apiary.pilotrun.app"},
		// Not shadowed: the same label under a different domain, which no
		// dispatch on this fleet claims.
		{ID: "m-other", Name: "api", Domain: "api.example.com"},
	}

	got := apiHostnameShadowed(machines, "api.pilotrun.app")

	want := map[string]bool{"m-shadowed": true, "m-upper": true, "m-custom": true}
	if len(got) != len(want) {
		t.Fatalf("reported %d machines, want %d: %+v", len(got), len(want), got)
	}
	for _, m := range got {
		if !want[m.ID] {
			t.Errorf("%s was reported, but its URL is still its own", m.ID)
		}
	}
}

// An operator alerts on the gauge; the log says which machine. Both have to
// follow the condition down again, or destroying the machine leaves an alert
// firing until someone restarts hostd to clear it.
func TestReportShadowedTracksTheConditionInBothDirections(t *testing.T) {
	shadowed := []state.Machine{{ID: "m-1", Name: "api", Domain: "api.pilotrun.app"}}

	key := reportShadowed(shadowed, "api.pilotrun.app", "")
	if got := metrics.APIHostnameShadowed.Load(); got != 1 {
		t.Errorf("gauge = %d with one machine shadowed, want 1", got)
	}
	if key == "" {
		t.Fatal("the reported key is empty, so an unchanged condition would log every pass")
	}

	// Same condition, next pass: the gauge holds, and the key is unchanged so
	// the caller does not log it again.
	if again := reportShadowed(shadowed, "api.pilotrun.app", key); again != key {
		t.Errorf("key changed to %q on an unchanged condition, want %q", again, key)
	}

	// Operator destroys the machine.
	if cleared := reportShadowed(nil, "api.pilotrun.app", key); cleared != "" {
		t.Errorf("key = %q after the condition cleared, want empty", cleared)
	}
	if got := metrics.APIHostnameShadowed.Load(); got != 0 {
		t.Errorf("gauge = %d after the condition cleared, want 0", got)
	}
}
