package netns

import (
	"testing"

	"github.com/google/nftables/expr"
)

// The activity rule observes and decides nothing. A verdict here would either
// drop a peer's traffic to a running replica or accept traffic past the tenant
// boundary, and both would be invisible until someone tried .internal.
func TestAnActivityRuleCountsAndNeverDecides(t *testing.T) {
	var counters int
	for _, e := range activityExprs(addr("fdcd:1::3")) {
		switch e.(type) {
		case *expr.Counter:
			counters++
		case *expr.Verdict:
			t.Error("the activity rule carries a verdict; it must only count")
		}
	}
	if counters != 1 {
		t.Errorf("activity rule has %d counters, want exactly 1", counters)
	}
}
