package router

import (
	"testing"
	"time"
)

// One window, on both paths.
//
// A same-host wake and a cross-host forward must bound a client's wait at the
// same number, or a client can tell where a machine is by how long it waits.
// That is the one thing the routing layer exists to hide, and it is exactly the
// kind of difference that survives review because each path looks reasonable
// alone.
func TestTheHeldWakeWindowIsOneNumberOnBothPaths(t *testing.T) {
	if forwardTimeout != HeldWakeWindow {
		t.Errorf("a forwarded request waits %s and a same-host wake waits %s; a "+
			"client could tell which host its machine is on from the difference",
			forwardTimeout, HeldWakeWindow)
	}
}

// The number is published on the website and written into the docs, so it is
// pinned here: changing it is a decision that has to travel with the sentences
// that quote it, not a constant somebody edits in passing.
func TestTheHeldWakeWindowIsTheOnePublished(t *testing.T) {
	if HeldWakeWindow != 120*time.Second {
		t.Errorf("HeldWakeWindow = %s, but the website and the skill both say 120 "+
			"seconds; change them together or not at all", HeldWakeWindow)
	}
}
