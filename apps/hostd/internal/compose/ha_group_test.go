package compose

import (
	"strings"
	"testing"
)

// haComposeText is what `pilot db ha` splices together: one Postgres service
// and one etcd service, sharing a build context because they share an image.
func haComposeText(t *testing.T) string {
	t.Helper()
	frag, err := HAFragmentFor("db", 3, 3)
	if err != nil {
		t.Fatalf("HAFragmentFor: %v", err)
	}
	sep, ok := frag.Etcd["x-pilots"].(map[string]any)["separate_machine"]
	if !ok || sep != true {
		t.Fatalf("the etcd service does not ask to be its own machine: %+v", frag.Etcd)
	}
	return `
name: demo
volumes:
  pgdata:
  db-etcd-data:
services:
  db:
    build: ./.pilots/db
    command: /usr/local/bin/pilot-patroni.sh
    volumes:
      - pgdata:/var/lib/postgresql/data
    deploy:
      replicas: 3
    x-pilots:
      engine: postgres
  db-etcd:
    build: ./.pilots/db
    command: /usr/local/bin/pilot-etcd.sh
    volumes:
      - db-etcd-data:/var/lib/etcd
    deploy:
      replicas: 3
    x-pilots:
      private: true
      engine: etcd
      separate_machine: true
`
}

// The etcd quorum is its own machines, not processes inside the database.
//
// etcd shares the database's build context because the recipe bakes Patroni,
// etcd and HAProxy into EVERY Postgres image and dispatches on PILOT_PG_ROLE at
// boot, so a cluster can be turned on without a rebuild. groupByContext read
// that shared context as "one machine with two processes" and folded the quorum
// into the database itself: `pilot db ha` deployed a cluster with NO etcd
// machines, having just told the user in its confirmation prompt that it would
// run three Postgres machines and three etcd machines on separate hosts.
// Dropping the separate_machine check collapses this to one step.
func TestTheEtcdQuorumIsItsOwnMachines(t *testing.T) {
	plan, planErr := planText(t, haComposeText(t))
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if len(plan.Steps) != 2 {
		names := []string{}
		for _, s := range plan.Steps {
			names = append(names, s.Name)
		}
		t.Fatalf("got %d steps (%s), want the database and its etcd as separate machines",
			len(plan.Steps), strings.Join(names, ", "))
	}
	for _, s := range plan.Steps {
		if len(s.Processes) != 0 {
			t.Errorf("%s was grouped: processes = %+v", s.Name, s.Processes)
		}
	}
}

// A service that shares a context and does NOT opt out still groups. The
// pooler is the case: `<name>-pool` carries no engine label and must stay a
// process inside its database, which is why the opt-out is written on the
// service that wants it rather than inferred from a label.
func TestAPoolerOnTheSameContextStillGroups(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  db:
    build: ./.pilots/db
    command: /usr/local/bin/pilot-patroni.sh
    x-pilots:
      engine: postgres
  db-pool:
    build: ./.pilots/db
    command: /usr/local/bin/pilot-pgbouncer.sh
    depends_on: [db]
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("got %d steps, want the pooler inside its database", len(plan.Steps))
	}
	if len(plan.Steps[0].Processes) != 2 {
		t.Errorf("processes = %+v, want the database and the pooler", plan.Steps[0].Processes)
	}
}
