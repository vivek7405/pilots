package netns

import (
	"net/netip"
	"testing"
)

// A flow from a machine that is no longer running must stop holding its peer
// up. Conntrack keeps an established TCP flow for five days by default, so a
// web app suspended with a pool open would otherwise keep its database awake
// forever -- the one thing the originator check exists to prevent.
func TestOnlyFlowsFromRunningPeersHoldAReplica(t *testing.T) {
	a, b, c, db := addr("fdcd:1::a"), addr("fdcd:1::b"), addr("fdcd:1::c"), addr("fdcd:1::d")
	running := map[netip.Addr]string{a: "m-a", c: "m-c", db: "m-db"}

	flows := []Flow{
		{Src: a, Dst: db},
		{Src: b, Dst: db}, // b is suspended: a stale socket, not activity
		{Src: c, Dst: db},
		{Src: a, Dst: addr("fdcd:9::9")}, // nothing running behind it
	}

	held := HeldBy(flows, running)
	if held["m-db"] != 2 {
		t.Errorf("held sessions on the database = %d, want 2", held["m-db"])
	}
	if len(held) != 1 {
		t.Errorf("held = %v, want only the database", held)
	}
}
