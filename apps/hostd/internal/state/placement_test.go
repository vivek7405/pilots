package state

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var placeNow = time.Unix(1_800_000_000, 0)

func cap4(id string, freeMiB, reclaimMiB, cpus int) HostCapacity {
	return HostCapacity{
		HostID: id, MemFreeMiB: freeMiB, MemReclaimableMiB: reclaimMiB,
		CPUCount: cpus, UpdatedAt: placeNow.Unix(),
	}
}

func hostSet(ids ...string) []Host {
	out := make([]Host, 0, len(ids))
	for _, id := range ids {
		out = append(out, Host{ID: id})
	}
	return out
}

func capsOf(rows ...HostCapacity) map[string]HostCapacity {
	out := map[string]HostCapacity{}
	for _, r := range rows {
		out[r.HostID] = r
	}
	return out
}

// Spread, never pack. A fleet of microVMs packed to 95% has no room to absorb
// the next burst, and the burst is exactly when the room is needed.
func TestPlacementPicksTheHostWithTheMostHeadroom(t *testing.T) {
	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024},
		placeNow,
		hostSet("host-a", "host-b"),
		capsOf(cap4("host-a", 4096, 0, 8), cap4("host-b", 8192, 0, 8)),
		nil, nil, 30*time.Second,
	)
	if len(got) == 0 || got[0] != "host-b" {
		t.Errorf("ranked %v, want the 8 GiB host first", got)
	}
	// And the tighter host is still a candidate: it can hold the machine, it
	// is simply not the first choice.
	if len(got) != 2 {
		t.Errorf("ranked %v, want both hosts offered", got)
	}
}

// Reclaimable memory counts. A host holding gigabytes of idle machines it
// would suspend anyway is not full, and refusing a create against it is
// refusing against a number that is about to change.
func TestReclaimableMemoryMakesAHostAvailable(t *testing.T) {
	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 4096},
		placeNow,
		hostSet("host-a"),
		capsOf(cap4("host-a", 512, 8192, 8)),
		nil, nil, 30*time.Second,
	)
	if len(got) != 1 || got[0] != "host-a" {
		t.Errorf("ranked %v; a host with 512 MiB free and 8 GiB reclaimable can hold 4 GiB", got)
	}
}

// A host that genuinely cannot hold the machine is not offered at all. The
// caller answers "no capacity" rather than sending it somewhere to fail.
func TestAHostThatCannotHoldTheMachineIsNotOffered(t *testing.T) {
	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 16384},
		placeNow,
		hostSet("host-a", "host-b"),
		capsOf(cap4("host-a", 1024, 512, 8), cap4("host-b", 2048, 0, 8)),
		nil, nil, 30*time.Second,
	)
	if len(got) != 0 {
		t.Errorf("ranked %v, want nothing: no host has 16 GiB", got)
	}
}

// The affinity bonus must break a near-tie and must NOT move a machine onto a
// host with meaningfully less room. Otherwise every machine piles onto
// whichever host happened to build things, which is bin-packing arrived at by
// accident.
func TestCachedBuildsBreakATieAndNothingMore(t *testing.T) {
	req := PlacementRequest{Name: "web", MemMiB: 1024, Builds: []string{"bld_1", "bld_2"}}
	equal := capsOf(cap4("host-a", 8192, 0, 8), cap4("host-b", 8192, 0, 8))

	t.Run("it breaks a tie", func(t *testing.T) {
		got := RankHosts(req, placeNow, hostSet("host-a", "host-b"), equal,
			map[string][]string{"host-b": {"bld_1", "bld_2", "bld_9"}}, nil, 30*time.Second)
		if len(got) == 0 || got[0] != "host-b" {
			t.Errorf("ranked %v, want the host holding both builds first", got)
		}
	})

	t.Run("half the builds is no bonus at all", func(t *testing.T) {
		// Half a cache is a download either way.
		got := RankHosts(req, placeNow, hostSet("host-a", "host-b"), equal,
			map[string][]string{"host-b": {"bld_1"}}, nil, 30*time.Second)
		if len(got) != 2 {
			t.Fatalf("ranked %v", got)
		}
		// Now it is an exact tie, resolved by the hash rather than by the
		// partial cache.
		byHash, _ := OwnerFor("web", hostSet("host-a", "host-b"))
		if got[0] != byHash {
			t.Errorf("ranked %v; an exact tie must fall to the hash (%s)", got, byHash)
		}
	})

	t.Run("it cannot outweigh real headroom", func(t *testing.T) {
		// host-a has 24 GiB more room. A cached build must not move the
		// machine onto host-b for it.
		got := RankHosts(req, placeNow, hostSet("host-a", "host-b"),
			capsOf(cap4("host-a", 32768, 0, 8), cap4("host-b", 8192, 0, 8)),
			map[string][]string{"host-b": {"bld_1", "bld_2"}}, nil, 30*time.Second)
		if len(got) == 0 || got[0] != "host-a" {
			t.Errorf("ranked %v; a cached build outweighed 24 GiB of headroom", got)
		}
	})
}

// A draining host is being emptied on purpose. Placing on it makes the drain
// chase its own tail and never converge.
func TestADrainingHostIsNeverPlacedOn(t *testing.T) {
	draining := cap4("host-b", 65536, 0, 8)
	draining.Draining = true

	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024},
		placeNow,
		hostSet("host-a", "host-b"),
		capsOf(cap4("host-a", 2048, 0, 8), draining),
		nil, nil, 30*time.Second,
	)
	if len(got) != 1 || got[0] != "host-a" {
		t.Errorf("ranked %v; the draining host must not be offered even with far more room", got)
	}
}

// A capacity row that has stopped being written describes a host that has
// stopped reporting. Placing on its last figures is placing on a guess.
func TestAStaleCapacityRowIsNotRanked(t *testing.T) {
	stale := cap4("host-b", 65536, 0, 8)
	stale.UpdatedAt = placeNow.Add(-10 * time.Minute).Unix()

	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024},
		placeNow,
		hostSet("host-a", "host-b"),
		capsOf(cap4("host-a", 2048, 0, 8), stale),
		nil, nil, 30*time.Second,
	)
	if len(got) != 1 || got[0] != "host-a" {
		t.Errorf("ranked %v; the stale host must not be offered", got)
	}
}

// A memory image carries raw CPUID and never restores across the Intel/AMD
// boundary. A create that restores one must land in its pool or fail deep
// inside Firecracker with nothing naming the vendor.
func TestARestoreIsRankedWithinItsVendorPool(t *testing.T) {
	vendors := map[string]string{
		"host-amd": "AuthenticAMD", "host-intel": "GenuineIntel",
	}
	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024, Vendor: "AuthenticAMD"},
		placeNow,
		hostSet("host-amd", "host-intel"),
		capsOf(cap4("host-amd", 2048, 0, 8), cap4("host-intel", 65536, 0, 8)),
		nil, vendors, 30*time.Second,
	)
	if len(got) != 1 || got[0] != "host-amd" {
		t.Errorf("ranked %v; an AMD image must not be offered an Intel host however much room it has", got)
	}

	// A create that BOOTS names no vendor and may go anywhere.
	anywhere := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024},
		placeNow,
		hostSet("host-amd", "host-intel"),
		capsOf(cap4("host-amd", 2048, 0, 8), cap4("host-intel", 65536, 0, 8)),
		nil, vendors, 30*time.Second,
	)
	if len(anywhere) != 2 {
		t.Errorf("a boot was ranked over %v, want both hosts", anywhere)
	}
}

// The ranking must not depend on the order rows arrived in. Two hosts reading
// the same fleet from their own replicas see it in different orders, and a
// ranking that changed with the order would send the same create two ways.
func TestTheRankingDoesNotDependOnInputOrder(t *testing.T) {
	caps := capsOf(
		cap4("host-a", 4096, 0, 8), cap4("host-b", 4096, 0, 8),
		cap4("host-c", 4096, 0, 8), cap4("host-d", 8192, 0, 8),
	)
	req := PlacementRequest{Name: "web", MemMiB: 1024}

	want := strings.Join(RankHosts(req, placeNow,
		hostSet("host-a", "host-b", "host-c", "host-d"), caps, nil, nil, 30*time.Second), ",")

	for _, order := range [][]string{
		{"host-d", "host-c", "host-b", "host-a"},
		{"host-c", "host-a", "host-d", "host-b"},
		{"host-b", "host-d", "host-a", "host-c"},
	} {
		got := strings.Join(RankHosts(req, placeNow, hostSet(order...), caps, nil, nil, 30*time.Second), ",")
		if got != want {
			t.Errorf("order %v ranked %s, want %s", order, got, want)
		}
	}
}

// Identical idle hosts must SPREAD. Falling to id order would send every
// create in the fleet to whichever host sorts first, which is the packing this
// whole function exists to avoid.
func TestIdenticalHostsSpreadRatherThanStackingOnTheFirstID(t *testing.T) {
	caps := capsOf(cap4("host-a", 8192, 0, 8), cap4("host-b", 8192, 0, 8), cap4("host-c", 8192, 0, 8))
	live := hostSet("host-a", "host-b", "host-c")

	chosen := map[string]int{}
	for i := 0; i < 60; i++ {
		got := RankHosts(PlacementRequest{Name: fmt.Sprintf("m-%d", i), MemMiB: 512},
			placeNow, live, caps, nil, nil, 30*time.Second)
		if len(got) == 0 {
			t.Fatalf("machine %d was placed nowhere", i)
		}
		chosen[got[0]]++
	}
	if len(chosen) < 2 {
		t.Errorf("60 creates over three identical hosts landed on %v; they must spread", chosen)
	}
}

// A host already tried and refused is not offered again, or a retry loop would
// walk back into the same 507.
func TestAnExcludedHostIsNotOfferedAgain(t *testing.T) {
	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024, Exclude: []string{"host-b"}},
		placeNow,
		hostSet("host-a", "host-b"),
		capsOf(cap4("host-a", 2048, 0, 8), cap4("host-b", 65536, 0, 8)),
		nil, nil, 30*time.Second,
	)
	if len(got) != 1 || got[0] != "host-a" {
		t.Errorf("ranked %v; the excluded host was offered again", got)
	}
}

// A host that has just joined has no capacity row yet. It must stay a
// candidate -- a fresh fleet has no rows at all and would otherwise be unable
// to place anything -- but it must never be PREFERRED over a host that has
// actually reported.
func TestAHostWithNoCapacityRowIsOfferedLast(t *testing.T) {
	got := RankHosts(
		PlacementRequest{Name: "web", MemMiB: 1024},
		placeNow,
		hostSet("host-a", "host-new"),
		capsOf(cap4("host-a", 2048, 0, 8)),
		nil, nil, 30*time.Second,
	)
	if len(got) != 2 {
		t.Fatalf("ranked %v, want both hosts", got)
	}
	if got[0] != "host-a" || got[1] != "host-new" {
		t.Errorf("ranked %v, want the reporting host first and the silent one last", got)
	}

	// A brand new fleet, where nobody has reported yet, still places.
	fresh := RankHosts(PlacementRequest{Name: "web", MemMiB: 1024},
		placeNow, hostSet("host-x", "host-y"), nil, nil, nil, 30*time.Second)
	if len(fresh) != 2 {
		t.Errorf("a fleet with no capacity rows placed nowhere: %v", fresh)
	}
}
