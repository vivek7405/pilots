package api

import (
	"context"
	"fmt"
)

// Which services may run several replicas on volumes.
//
// # The rule, and why it has an exception at all
//
// A volume-backed service runs ONE replica, because two machines mounting one
// volume is two processes writing one filesystem. That is not a limitation to
// be worked around; it is what a volume is.
//
// The exception is a service whose write-once `pilot.engine` label is an engine
// that does its OWN replication: a Patroni-managed Postgres, or an etcd. Those
// do not share a volume. Each ordinal gets its own, and the engine replicates
// between them, which is a different arrangement that happens to be spelled
// with the same number.
//
// # Why the LABEL and not a flag
//
// The label is written once, by the recipe, at create. It cannot be added to an
// existing service, which means a hand-written service cannot reach this by
// editing a number: it is refused, and the refusal names the recipe that would
// have written the configuration this actually needs. A flag anybody could set
// would let somebody scale a plain Postgres to three machines, each with its
// own empty disk, and find out what that means later.

// replicatingEngines are the engines that replicate between their own
// ordinals, with the ceiling each is allowed.
//
// Not arbitrary. An even etcd has no majority it did not already have at one
// fewer member, so it buys failure modes and no availability. A Postgres past
// seven is a replication fan-out nobody should reach for without saying why,
// and refusing it is how they get asked.
var replicatingEngines = map[string]struct {
	max     int
	oddOnly bool
}{
	"postgres": {max: 7},
	"etcd":     {max: 9, oddOnly: true},
}

// EngineOf is the write-once engine label on a service or machine, or empty.
func (d Deps) EngineOf(ctx context.Context, id string) string {
	labels, err := d.Store.GetLabels(ctx, id)
	if err != nil || labels == nil {
		return ""
	}
	return labels.Labels["pilot.engine"]
}

// checkReplicatedVolumes decides whether this service may run this many
// replicas on volumes, and says why not.
//
// The error is the product here. "Refused" teaches nothing; naming the recipe
// that writes a scalable Postgres is the difference between somebody giving up
// and somebody running the right command.
func checkReplicatedVolumes(engine string, replicas int) error {
	if replicas <= 1 {
		return nil
	}
	limits, ok := replicatingEngines[engine]
	if !ok {
		if engine != "" {
			return fmt.Errorf("a %s service runs one replica: it mounts a volume, "+
				"and a volume is mounted by one machine at a time", engine)
		}
		return fmt.Errorf("a service that mounts a volume runs exactly one replica: " +
			"a volume is mounted by one machine at a time. A hand-written postgres " +
			"service cannot scale; `pilot add postgres` writes the recipe that can, " +
			"then `pilot db ha enable`")
	}
	if replicas > limits.max {
		return fmt.Errorf("%d replicas of a %s; the ceiling is %d, because past "+
			"that is a replication fan-out worth saying out loud",
			replicas, engine, limits.max)
	}
	if limits.oddOnly && replicas%2 == 0 {
		return fmt.Errorf("%d %s members; an even number has no majority it did not "+
			"already have at %d, so it buys failure modes and no availability",
			replicas, engine, replicas-1)
	}
	return nil
}

// nextReplicaHint is what to do about a refusal, for the `next` field.
func nextReplicaHint(engine string) string {
	if _, ok := replicatingEngines[engine]; ok {
		return "pick a count within the engine's own limits"
	}
	return "`pilot add postgres` writes a recipe that can scale, then `pilot db ha enable`"
}
