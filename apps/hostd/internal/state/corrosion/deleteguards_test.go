package corrosion

import (
	"context"
	"errors"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// A Delete* is a write, and it is guarded exactly as its Put* is. Five tables
// had a guarded Put* beside an unguarded Delete*, so a caller that assumed
// the two agreed got a cross-host row write through a CRDT merge (invariant
// 1), and a caller that assumed the reverse got ErrNotOwner mid-teardown.
// These pin the guards on the two tables whose rule needs no arbiter hash to
// state: a host's own egress, and a machine's owner.
func TestDeleteHostEgressRefusesAnotherHost(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO host_egress (host_id, prefix6, interface, updated_at)
		VALUES ('host-b','fd00:b::/64','eth0',1)`)

	err := store.DeleteHostEgress(context.Background(), "host-b")
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("DeleteHostEgress of another host's row returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM host_egress WHERE host_id='host-b'`); got != "host-b" {
		t.Errorf("the row was deleted anyway: host_id=%q", got)
	}
}

func TestDeleteLineageRefusesAnotherHostsMachine(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)
	agent.exec(t, `INSERT INTO machine_lineage (id, parent_id, checkpoint_id, mem_build_id,
		rootfs_build_id, volume_snapshot, created_at) VALUES ('m-1','m-0','','','','',1)`)

	err := store.DeleteLineage(context.Background(), "m-1")
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("DeleteLineage of another host's machine returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT id FROM machine_lineage WHERE id='m-1'`); got != "m-1" {
		t.Errorf("the row was deleted anyway: id=%q", got)
	}
}
