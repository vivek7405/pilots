package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A machine's size was decided once, at create, and could never change. The
// only way to give an application more memory was to destroy it and make
// another, which means a new id, a new URL and whatever was on its disk gone.
func TestResizeChangesTheSizeAndKeepsTheMachine(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := postJSON(t, h, "/v1/machines/m_1/resize", testKey, `{"vcpus":4,"mem_mib":4096}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if fake.resizedTo != [2]int{4, 4096} {
		t.Errorf("the manager was asked for %v, want {4 4096}", fake.resizedTo)
	}
	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VCPUs != 4 || got.MemMiB != 4096 {
		t.Errorf("machine = %d vCPU / %d MiB, want 4 / 4096", got.VCPUs, got.MemMiB)
	}
	// The identity survives, which is the whole difference between a resize
	// and making a new machine.
	if got.ID != "m_1" {
		t.Errorf("id = %q; a resize keeps the machine", got.ID)
	}
}

// Either dimension alone, which is how "give it more memory" is said without
// restating the vCPU count.
func TestResizeAcceptsOneDimension(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	if rec := postJSON(t, h, "/v1/machines/m_1/resize", testKey, `{"mem_mib":2048}`); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if fake.resizedTo != [2]int{0, 2048} {
		t.Errorf("the manager was asked for %v, want {0 2048}", fake.resizedTo)
	}
}

func TestResizeRefusesAnEmptyRequest(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	for _, body := range []string{`{}`, `{"vcpus":0,"mem_mib":0}`, `not json`} {
		rec := postJSON(t, h, "/v1/machines/m_1/resize", testKey, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q got %d, want 400", body, rec.Code)
		}
	}
}

// Only the INCREASE counts against the quota. A caller shrinking a machine
// must not be refused for being over a limit it is in the middle of getting
// under, which is the one moment the limit is most in the way.
func TestShrinkingIsNotRefusedByAFullQuota(t *testing.T) {
	h, st, fake := newTestServerWithManager(t)
	ctx := context.Background()

	// The machine is 4 vCPU, and the org's limit is 2: it is already over,
	// which is exactly the state a shrink is meant to fix.
	fake.machine.VCPUs, fake.machine.MemMiB = 4, 4096
	if err := st.PutMachine(ctx, &state.Machine{
		ID: "m_1", Name: "webapp", HostID: "host-test", State: "running",
		VCPUs: 4, MemMiB: 4096, Domain: "webapp.pilotrun.app",
		UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: "m_1", Kind: "machine", OrgID: "org_1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutQuota(ctx, &state.Quota{
		OrgID: "org_1", MaxMachines: 10, MaxVCPUs: 2, MaxMemMiB: 1024,
		MaxVolumeGiB: 10, MaxBuilds: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, h, "/v1/machines/m_1/resize", testKey, `{"vcpus":1,"mem_mib":512}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a shrink was refused with %d: %s", rec.Code, rec.Body.String())
	}
}
