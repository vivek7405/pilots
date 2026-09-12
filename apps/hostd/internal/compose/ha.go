package compose

import "fmt"

// Turning one Postgres into a Patroni cluster, without changing the image.
//
// # Why one image and three roles
//
// A conversion that changed the image would be a rebuild, a new release and a
// window where the old nodes and the new ones are not the same software. The
// recipe image carries Patroni, etcd and HAProxy in every mode, and which
// processes run is picked at start from PILOT_PG_ROLE. So enabling high
// availability changes an environment variable and a replica count, and
// nothing about what is running is a guess.
//
// The cost is an image with three binaries most databases never use. That is
// paid once, in a layer that caches, against a conversion path that would
// otherwise have a rebuild in the middle of it.
//
// # Why etcd is its own service
//
// Co-locating it on the data nodes was the obvious saving and it does not work:
// the smallest sensible cluster is two data nodes and three etcd members, which
// cannot co-locate, and a lost data node must not also shrink the quorum that
// decides whether to replace it.
//
// It is the customer's consensus inside the customer's app. Nothing in hostd
// reads it, which is what keeps this from becoming a control plane.

// The roles a recipe machine can start in.
const (
	// RoleSingle is one Postgres with a pooler beside it: what `pilot add`
	// writes and what most databases stay as.
	RoleSingle = "single"
	// RoleData is a Patroni-managed Postgres, a pooler on 6433 and HAProxy on
	// the published 6432 following whichever node is primary.
	RoleData = "data"
	// RoleEtcd is one etcd member and nothing else.
	RoleEtcd = "etcd"
)

// Bounds on a cluster's shape.
//
// Not arbitrary. An even etcd has no majority it did not already have at one
// fewer member, so it buys failure modes and no availability. A Postgres past
// seven is a replication fan-out nobody should reach for without saying why,
// and refusing it is how they get asked.
const (
	MinDataReplicas = 2
	MaxDataReplicas = 7
	MinEtcdMembers  = 3
	MaxEtcdMembers  = 9
)

// ValidateHAShape refuses a cluster that cannot work before anything is
// written.
func ValidateHAShape(replicas, etcd int) error {
	if replicas < MinDataReplicas || replicas > MaxDataReplicas {
		return fmt.Errorf("compose: %d data replicas; a Patroni cluster wants %d to %d",
			replicas, MinDataReplicas, MaxDataReplicas)
	}
	if etcd%2 == 0 {
		return fmt.Errorf("compose: %d etcd members; an even number has no majority "+
			"it did not already have at %d, so it buys failure modes and no availability",
			etcd, etcd-1)
	}
	if etcd < MinEtcdMembers || etcd > MaxEtcdMembers {
		return fmt.Errorf("compose: %d etcd members; wanted an odd number from %d to %d",
			etcd, MinEtcdMembers, MaxEtcdMembers)
	}
	return nil
}

// HAFragment is the compose text a conversion splices in.
//
// Returned as data rather than written here, because the thing that edits a
// person's compose file is the CLI, on their machine, where they can read the
// diff before it is deployed.
type HAFragment struct {
	// Service is what replaces the database's own block's changing half: the
	// replica count, the role, and what it now depends on.
	Service map[string]any
	// Etcd is the new service, by name.
	EtcdName    string
	Etcd        map[string]any
	EtcdVolume  string
	SecretNames []string
	// Statement is what the operator is told before any of it happens.
	Statement string
}

// HAFragmentFor builds the conversion for one database.
func HAFragmentFor(name string, replicas, etcd int) (*HAFragment, error) {
	if err := ValidateHAShape(replicas, etcd); err != nil {
		return nil, err
	}
	etcdName := name + "-etcd"

	return &HAFragment{
		Service: map[string]any{
			"deploy": map[string]any{"replicas": replicas},
			"environment": map[string]any{
				"PILOT_PG_ROLE": RoleData,
				// A second secret, generated like the first and never sent:
				// the replication user is not the superuser, so a leaked
				// replication password cannot write.
				"PATRONI_REPLICATION_PASSWORD": "secret://patroni_replication",
				"PILOT_ETCD_HOSTS":             etcdName + ".internal:2379",
			},
			// etcd first, because a Patroni that starts with no consensus to
			// reach spends its startup failing rather than waiting.
			"depends_on": []string{etcdName},
		},
		EtcdName: etcdName,
		Etcd: map[string]any{
			"build":       "./.pilots/" + name,
			"environment": map[string]any{"PILOT_PG_ROLE": RoleEtcd},
			"deploy":      map[string]any{"replicas": etcd},
			"volumes":     []string{etcdName + "-data:/var/lib/etcd"},
			"healthcheck": healthcheck("etcdctl endpoint health"),
			"x-pilots": map[string]any{
				"private":        true,
				"durable_volume": true,
				// Labelled, because the replica rule reads this label and an
				// etcd that could not scale past one would be a quorum of one.
				"engine":   "etcd",
				"size_gib": 1,
			},
		},
		EtcdVolume:  etcdName + "-data",
		SecretNames: []string{"patroni_replication"},
		Statement: fmt.Sprintf(
			"This runs %d Postgres machines and %d etcd machines, %d in total, "+
				"each on its own volume and placed on a different host where the "+
				"fleet has one. Patroni decides which node is primary and pilots "+
				"does not participate in that decision: you operate the database, "+
				"we operate the machines it runs on.",
			replicas, etcd, replicas+etcd),
	}, nil
}
