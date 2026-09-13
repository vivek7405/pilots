package state

import (
	"fmt"
	"testing"
	"time"
)

// Hosts a few MiB apart are one pool, not a ranking.
//
// # The bug
//
// The tie that triggers the hash spread was exact float equality. Scores come
// from MemAvailable/1024, which moves by megabytes between heartbeats on any
// host doing work, so two interchangeable hosts scored differently every time
// and the spread never ran. The fleet degenerated into "whichever host is
// momentarily emptiest takes everything", which is the bin-packing this file
// exists to avoid.
//
// # Why the existing test could not catch it
//
// TestIdenticalHostsSpreadRatherThanStackingOnTheFirstID builds three hosts
// with the SAME numbers, so their floats are equal by construction. Identical
// inputs are precisely the case that needs no tolerance. This one differs them
// by the noise a real heartbeat carries, which is the case that was broken.
func TestHostsAFewMiBApartStillSpread(t *testing.T) {
	// Three MiB between best and worst: noise, not a decision.
	caps := capsOf(cap4("host-a", 8192, 0, 8), cap4("host-b", 8190, 0, 8),
		cap4("host-c", 8189, 0, 8))
	live := hostSet("host-a", "host-b", "host-c")

	chosen := map[string]int{}
	for i := range 60 {
		got := RankHosts(PlacementRequest{Name: fmt.Sprintf("m-%d", i), MemMiB: 512},
			placeNow, live, caps, nil, nil, 30*time.Second)
		if len(got) == 0 {
			t.Fatalf("machine %d was placed nowhere", i)
		}
		chosen[got[0]]++
	}
	if len(chosen) < 2 {
		t.Fatalf("60 creates across three hosts three MiB apart all went to one "+
			"host (%v). A few MiB of heartbeat noise must not decide placement; "+
			"hosts this close are one pool, and the name's hash is what picks "+
			"among them", chosen)
	}
}

// The tolerance must not swallow a difference that SHOULD decide.
//
// Without this half, the fix would be indistinguishable from deleting the
// ranking: a host with gigabytes more room has to win every time, whatever the
// name hashes to.
func TestARealDifferenceStillDecides(t *testing.T) {
	caps := capsOf(cap4("host-a", 32768, 0, 8), cap4("host-b", 4096, 0, 8),
		cap4("host-c", 4096, 0, 8))
	live := hostSet("host-a", "host-b", "host-c")

	for i := range 20 {
		got := RankHosts(PlacementRequest{Name: fmt.Sprintf("m-%d", i), MemMiB: 512},
			placeNow, live, caps, nil, nil, 30*time.Second)
		if len(got) == 0 {
			t.Fatalf("machine %d was placed nowhere", i)
		}
		if got[0] != "host-a" {
			t.Fatalf("machine %d went to %s, but host-a has 28 GiB more room; a "+
				"tolerance meant for heartbeat noise must not reach a difference "+
				"this size", i, got[0])
		}
	}
}

// A cached build still outranks a host that is merely a little emptier.
//
// The tolerance sits between heartbeat noise and the affinity bonus, and the
// bonus is what makes a deploy a restore instead of a download. Widening the
// tolerance until it swallowed the bonus would cost that silently.
func TestACachedBuildStillBeatsASlightlyEmptierHost(t *testing.T) {
	// host-b is 200 MiB emptier, which is well past the tolerance and well
	// short of the bonus.
	caps := capsOf(cap4("host-a", 8192, 0, 8), cap4("host-b", 8392, 0, 8))
	live := hostSet("host-a", "host-b")
	cached := map[string][]string{"host-a": {"build-1"}}

	got := RankHosts(PlacementRequest{Name: "web", MemMiB: 512, Builds: []string{"build-1"}},
		placeNow, live, caps, cached, nil, 30*time.Second)
	if len(got) == 0 || got[0] != "host-a" {
		t.Fatalf("ranked %v; the host holding the build must win over one that is "+
			"200 MiB emptier, or a deploy pays a download it did not need to", got)
	}
}

// Every host in the fleet must rank a create identically, or two of them
// disagree about who owns it and both act on their own answer.
func TestTheTiedSetIsTheSameWhoeverComputesIt(t *testing.T) {
	caps := capsOf(cap4("host-a", 8192, 0, 8), cap4("host-b", 8190, 0, 8),
		cap4("host-c", 8189, 0, 8))
	req := PlacementRequest{Name: "alpha", MemMiB: 512}

	want := RankHosts(req, placeNow, hostSet("host-a", "host-b", "host-c"),
		caps, nil, nil, 30*time.Second)
	// The same rows arriving in a different order, which is what another
	// host's slice looks like.
	got := RankHosts(req, placeNow, hostSet("host-c", "host-a", "host-b"),
		caps, nil, nil, 30*time.Second)

	if fmt.Sprint(want) != fmt.Sprint(got) {
		t.Fatalf("two hosts disagree about the ranking: %v vs %v. Both would then "+
			"act on their own answer for the same create", want, got)
	}
}
