package services

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func hosts(ids ...string) []state.Host {
	out := make([]state.Host, 0, len(ids))
	for _, id := range ids {
		out = append(out, state.Host{ID: id})
	}
	return out
}

// The property the whole placement exists for: replicas of one service on one
// host are replicas that die together.
func TestOrdinalsLandOnDistinctHostsWhileThereAreEnough(t *testing.T) {
	live := hosts("host-c", "host-a", "host-b")
	for replicas := 2; replicas <= 3; replicas++ {
		placed := OrdinalHostsFor("svc_1", replicas, live)
		seen := map[string]bool{}
		for _, host := range placed {
			if seen[host] {
				t.Errorf("%d replicas placed twice on %s: %v", replicas, host, placed)
			}
			seen[host] = true
		}
	}
}

// Two hosts computing this must agree without talking. An arrangement that
// needs a conversation needs a coordinator, and rule 1 forbids one.
func TestPlacementIsDeterministicAndOrderIndependent(t *testing.T) {
	first := OrdinalHostsFor("svc_1", 3, hosts("host-a", "host-b", "host-c"))
	// The same fleet, listed in a different order, as two hosts' replicas
	// genuinely would be.
	second := OrdinalHostsFor("svc_1", 3, hosts("host-c", "host-a", "host-b"))
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("ordinal %d went to %s and to %s: the answer depends on the "+
				"order rows arrived in", i+1, first[i], second[i])
		}
	}
}

// Two services must not stack their ordinals on the same host, or a fleet of
// three hosts runs every cluster's leader on one of them.
func TestTwoServicesDoNotStartOnTheSameHost(t *testing.T) {
	live := hosts("host-a", "host-b", "host-c")
	a := OrdinalHostsFor("svc_aaa", 1, live)
	b := OrdinalHostsFor("svc_bbb", 1, live)
	c := OrdinalHostsFor("svc_ccc", 1, live)
	if a[0] == b[0] && b[0] == c[0] {
		t.Errorf("three services all start on %s; the base is not derived from "+
			"the service id", a[0])
	}
}

// A fleet smaller than the replica count wraps rather than failing. That is a
// real fleet being too small, which is the operator's problem to see, not a
// reason to refuse to run.
func TestASmallFleetWrapsRatherThanRefusing(t *testing.T) {
	placed := OrdinalHostsFor("svc_1", 3, hosts("host-a", "host-b"))
	if len(placed) != 3 {
		t.Fatalf("placed %d of 3", len(placed))
	}
	for i, host := range placed {
		if host == "" {
			t.Errorf("ordinal %d was placed nowhere", i+1)
		}
	}
	// Ordinals 1 and 3 share a host on a fleet of two, which is arithmetic
	// rather than a bug: the point is that it wraps evenly instead of piling
	// everything onto one.
	if placed[0] == placed[1] {
		t.Error("two ordinals landed on one host while the other sat empty")
	}
}

// An empty fleet places nothing rather than panicking on a modulo by zero. A
// host with no live peers is a real state during a start-up.
func TestAnEmptyFleetPlacesNothing(t *testing.T) {
	if got := ordinalHost("svc_1", 1, nil); got != "" {
		t.Errorf("placed on %q with no live hosts", got)
	}
}

// Only the two engines that actually replicate between their own volumes take
// this path. A short list is what makes adding one a decision.
func TestOnlyTheEnginesThatReplicateTakeTheOrdinalPath(t *testing.T) {
	for _, engine := range []string{"postgres", "etcd"} {
		if !replicatesItsOwnOrdinals(engine) {
			t.Errorf("%s does not take the ordinal path", engine)
		}
	}
	for _, engine := range []string{"", "mysql", "redis", "mongo", "anything"} {
		if replicatesItsOwnOrdinals(engine) {
			t.Errorf("%s takes the ordinal path, and would get several machines "+
				"each with its own empty disk", engine)
		}
	}
}
