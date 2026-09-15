package api

import (
	"strings"
	"testing"
)

// One replica is always fine, whatever the service is. The rule is about
// SEVERAL machines on volumes, and a rule that fired at one would refuse every
// volume-backed service there is.
func TestOneReplicaIsNeverRefused(t *testing.T) {
	for _, engine := range []string{"", "postgres", "etcd", "mysql"} {
		if err := checkReplicatedVolumes(engine, 1); err != nil {
			t.Errorf("%q at one replica: %v", engine, err)
		}
		if err := checkReplicatedVolumes(engine, 0); err != nil {
			t.Errorf("%q at zero replicas: %v", engine, err)
		}
	}
}

// The refusal is the product. "Refused" teaches nothing; naming the recipe is
// the difference between somebody giving up and somebody running the right
// command.
func TestAHandWrittenServiceIsRefusedAndToldWhatWouldWork(t *testing.T) {
	err := checkReplicatedVolumes("", 3)
	if err == nil {
		t.Fatal("a hand-written volume service scaled to three")
	}
	if !strings.Contains(err.Error(), "pilot add postgres") {
		t.Errorf("the refusal does not name the recipe: %v", err)
	}
	if !strings.Contains(nextReplicaHint(""), "pilot db ha enable") {
		t.Errorf("the next step does not name the command: %s", nextReplicaHint(""))
	}
}

// An engine that does NOT replicate between its own ordinals is refused too,
// and told why rather than pointed at a recipe that would not help it: three
// MySQL machines each with an empty disk is not a cluster.
func TestAnEngineThatDoesNotReplicateIsStillRefused(t *testing.T) {
	err := checkReplicatedVolumes("mysql", 3)
	if err == nil {
		t.Fatal("mysql scaled onto volumes")
	}
	if !strings.Contains(err.Error(), "mysql") {
		t.Errorf("the refusal does not name the engine: %v", err)
	}
}

func TestPostgresScalesWithinItsCeiling(t *testing.T) {
	for _, n := range []int{2, 3, 7} {
		if err := checkReplicatedVolumes("postgres", n); err != nil {
			t.Errorf("postgres at %d replicas: %v", n, err)
		}
	}
	err := checkReplicatedVolumes("postgres", 8)
	if err == nil {
		t.Fatal("postgres scaled past its ceiling")
	}
	if !strings.Contains(err.Error(), "7") {
		t.Errorf("the refusal does not name the ceiling: %v", err)
	}
}

// An even etcd has no majority it did not already have at one fewer member, so
// it buys failure modes and no availability. The refusal says that, because
// "even numbers are refused" reads as a quirk rather than as a reason.
func TestAnEvenEtcdIsRefusedWithTheReason(t *testing.T) {
	for _, n := range []int{3, 5, 7, 9} {
		if err := checkReplicatedVolumes("etcd", n); err != nil {
			t.Errorf("etcd at %d members: %v", n, err)
		}
	}
	err := checkReplicatedVolumes("etcd", 4)
	if err == nil {
		t.Fatal("an even etcd was admitted")
	}
	if !strings.Contains(err.Error(), "majority") {
		t.Errorf("the refusal does not give the reason: %v", err)
	}

	if err := checkReplicatedVolumes("etcd", 11); err == nil {
		t.Error("etcd scaled past its ceiling")
	}
}
