package corrosion

import (
	"encoding/json"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A deleted cpu row leaves the cache too.
//
// Every host materializes machine_cpu into a map that the router and the
// rescue ranking read from memory, so a row DeleteMachineCPU removed but the
// cache kept is the leak the delete exists to close, once per host. The
// subscription reports the delete; this pins that the cache acts on it.
func TestACacheDropsADeletedMachineCPURow(t *testing.T) {
	// Built directly rather than through NewCache: apply is the whole subject
	// here, and NewCache wants a live agent to subscribe against.
	c := &Cache{machineCPU: map[string]state.MachineCPU{}}

	change := func(kind ChangeKind, id, vendor string) Change {
		vals := make([]json.RawMessage, 0, 5)
		for _, v := range []any{id, state.KindMachine, vendor, "", 0} {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			vals = append(vals, b)
		}
		return Change{Kind: kind, Values: vals}
	}

	c.apply("machine_cpu", change(ChangeInsert, "m-1", "AuthenticAMD"))
	c.apply("machine_cpu", change(ChangeInsert, "m-2", "GenuineIntel"))
	if got := c.MachineVendor("m-1"); got != "AuthenticAMD" {
		t.Fatalf("the insert did not land: vendor = %q", got)
	}

	c.apply("machine_cpu", change(ChangeDelete, "m-1", "AuthenticAMD"))
	if _, ok := c.MachineCPU("m-1"); ok {
		t.Error("a deleted cpu row is still in the cache")
	}
	if got := c.MachineVendor("m-1"); got != "" {
		t.Errorf("a deleted machine still reports vendor %q", got)
	}
	// The counterfactual: a delete that cleared the map would pass the above.
	if got := c.MachineVendor("m-2"); got != "GenuineIntel" {
		t.Errorf("deleting m-1 took m-2 with it: vendor = %q", got)
	}
}
