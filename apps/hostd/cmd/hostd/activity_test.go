package main

import (
	"reflect"
	"testing"
)

// The counters are read off a table that is deleted and rebuilt whenever the
// fleet moves, so every count returns to zero on a rebuild. Reading a reset as
// traffic would touch every replica's row on the tick after any machine was
// created, which is exactly the signal the autoscaler trusts to mean "busy".
func TestARisingCountIsActivityAndAResetIsNot(t *testing.T) {
	seen := map[string]uint64{}

	if got := risen(seen, map[string]uint64{"m-1": 5}); len(got) != 0 {
		t.Errorf("a first reading reported %v; it only sets the baseline", got)
	}
	if got := risen(seen, map[string]uint64{"m-1": 5}); len(got) != 0 {
		t.Errorf("an unchanged count reported %v", got)
	}
	if got := risen(seen, map[string]uint64{"m-1": 9}); !reflect.DeepEqual(got, []string{"m-1"}) {
		t.Errorf("a rising count reported %v, want [m-1]", got)
	}
	// The table was rebuilt: the counter is back to zero and nothing arrived.
	if got := risen(seen, map[string]uint64{"m-1": 0}); len(got) != 0 {
		t.Errorf("a rebuilt table reported %v as activity", got)
	}
	if got := risen(seen, map[string]uint64{"m-1": 2}); !reflect.DeepEqual(got, []string{"m-1"}) {
		t.Errorf("traffic after a rebuild reported %v, want [m-1]", got)
	}

	// A machine with no rule any more is forgotten, so a slot reused by a
	// different machine cannot inherit its baseline.
	if got := risen(seen, map[string]uint64{}); len(got) != 0 {
		t.Errorf("an empty reading reported %v", got)
	}
	if _, still := seen["m-1"]; still {
		t.Error("a machine with no rule kept its baseline")
	}
}
