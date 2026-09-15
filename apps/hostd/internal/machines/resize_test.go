package machines

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// putResizable stores a plain running machine on this host, the shape a resize
// is allowed to act on.
func putResizable(t *testing.T, st state.Store, row *state.Machine) {
	t.Helper()
	if row.ID == "" {
		row.ID = "m_size"
	}
	if row.HostID == "" {
		row.HostID = "host-a"
	}
	if row.State == "" {
		row.State = StateRunning
	}
	if row.VCPUs == 0 {
		row.VCPUs = 1
	}
	if row.MemMiB == 0 {
		row.MemMiB = 512
	}
	row.UpdatedAt = time.Now().Unix()
	if err := st.PutMachine(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}

// A replica belongs to its service. Resizing one directly would leave the
// fleet serving one request in three from a machine of a different size, and
// the next rollout would put it back without saying anything, because a
// replica is created at the SERVICE's size.
//
// So the refusal is not a limitation, it is the only answer that stays true
// after the next deploy, and it names the operation that does work.
func TestResizeRefusesAServiceReplicaAndNamesTheRightCommand(t *testing.T) {
	m, st := storeManager(t)
	putResizable(t, st, &state.Machine{ID: "m_rep", ServiceID: "svc_api"})

	_, err := m.Resize(t.Context(), "m_rep", 4, 4096)
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "pilot services scale") {
		t.Errorf("the refusal does not name the command that works: %v", err)
	}
	// And it changed nothing on the way out.
	row, err := st.GetMachine(t.Context(), "m_rep")
	if err != nil {
		t.Fatal(err)
	}
	if row.VCPUs != 1 || row.MemMiB != 512 {
		t.Errorf("the row moved to %d/%d on a refused resize", row.VCPUs, row.MemMiB)
	}
}

// A checkpoint is a memory image, and a memory image is fixed at the size it
// was photographed. Resizing a machine that has one would leave the caller
// holding restore points that can never be restored -- which looks like a
// working backup right up to the moment it is needed.
func TestResizeRefusesAMachineThatHasCheckpoints(t *testing.T) {
	m, st := storeManager(t)
	putResizable(t, st, &state.Machine{ID: "m_ck"})
	if err := st.PutCheckpoint(t.Context(), &state.Checkpoint{
		ID: "ck_1", MachineID: "m_ck", Seq: 1, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := m.Resize(t.Context(), "m_ck", 2, 1024)
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "checkpoint") {
		t.Errorf("the refusal does not say what is in the way: %v", err)
	}
}

// Resizing to the size it already is must not cost the machine its memory.
//
// The obvious implementation kills and boots unconditionally, so a caller
// re-applying its desired size -- a reconciler, a retried request, a `scale`
// run twice -- would restart the application every time for no reason at all.
func TestResizeToTheSameSizeIsANoOp(t *testing.T) {
	m, st := storeManager(t)
	putResizable(t, st, &state.Machine{ID: "m_same", VCPUs: 2, MemMiB: 1024})

	row, err := m.Resize(t.Context(), "m_same", 2, 1024)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if row.State != StateRunning {
		t.Errorf("state = %q; a no-op resize must not take the machine down", row.State)
	}
	// Zero on both dimensions is the same no-op said the other way.
	if _, err := m.Resize(t.Context(), "m_same", 0, 0); err != nil {
		t.Errorf("an empty resize: %v", err)
	}
}

// A resize is a write, so it obeys the single-writer rule the same way every
// other write on a machine does: only the host holding the machine may make
// it. A second host resizing a row it does not own corrupts through a merge
// rather than erroring.
func TestResizeRefusesAMachineThisHostDoesNotHold(t *testing.T) {
	m, st := storeManager(t)
	putResizable(t, st, &state.Machine{ID: "m_far", HostID: "host-b"})

	if _, err := m.Resize(t.Context(), "m_far", 4, 4096); !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
}

// The bounds exist so a typo is refused at the edge rather than somewhere deep
// in a boot, where it costs the machine its memory before it fails.
func TestValidateSizeBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		vcpus, mem  int
		wantRefusal bool
	}{
		{"a normal size", 4, 4096, false},
		{"the largest allowed", MaxVCPUs, MaxMemMiB, false},
		// A PARTIAL size is refused here, and that is the change. validateSize
		// now takes a RESOLVED size -- both fields filled in from the row --
		// so "one field named, the other zero" is a shape it should never see.
		// Accepting it is what let the floor be skipped: the check ran before
		// the fill, saw `0 vCPUs, 32 MiB`, read it as a partial and returned
		// without checking anything. The edge check that a partial must pass
		// is validateBounds, asserted below.
		{"only one dimension named", 0, 2048, true},
		{"neither dimension named", 0, 0, false},
		{"more vCPUs than any host has", MaxVCPUs + 1, 1024, true},
		{"more memory than any host has", 4, MaxMemMiB + 1, true},
		{"negative", -1, 1024, true},
		{"too little memory to boot a guest", 1, 64, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSize(tc.vcpus, tc.mem)
			if tc.wantRefusal && err == nil {
				t.Errorf("%d vCPU / %d MiB was accepted", tc.vcpus, tc.mem)
			}
			if !tc.wantRefusal && err != nil {
				t.Errorf("%d vCPU / %d MiB was refused: %v", tc.vcpus, tc.mem, err)
			}
			if tc.wantRefusal && err != nil && !errors.Is(err, ErrInvalid) {
				t.Errorf("refusal is %v, want ErrInvalid so the API answers 400", err)
			}
		})
	}
}

// The refusals above say 400, and that has to be true where it is DECIDED.
//
// ErrInvalid was a bare errors.New, and api.mapError recognises wrapped
// sentinels and nothing else -- so every refusal in this package reached the
// caller as HTTP 500 "internal error" with the reason buried in
// `details.cause`, while the test above happily asserted ErrInvalid and the doc
// comment happily said 400. An assertion about a sentinel is not an assertion
// about a status. This is the one that is.
func TestEveryRefusalThisPackageMakesIsABadRequest(t *testing.T) {
	if !errors.Is(ErrInvalid, api.ErrBadRequest) {
		t.Fatal("ErrInvalid does not wrap api.ErrBadRequest, so api.mapError " +
			"cannot see it and every caller mistake this package refuses comes " +
			"back as a 500 inviting a retry that can never work")
	}
	// And it stays distinguishable from the other sentinel, which is the whole
	// reason there are two of them.
	if errors.Is(ErrInvalid, state.ErrNotFound) {
		t.Error("ErrInvalid also reads as not-found; a bad size would answer 404")
	}
	for _, err := range []error{
		validateSize(MaxVCPUs+1, 1024),
		validateSize(-1, 1024),
		validateSize(1, 64),
	} {
		if !errors.Is(err, api.ErrBadRequest) {
			t.Errorf("%v does not reach the mapper as a bad request", err)
		}
	}
}

// A partial resize passes the EDGE check and is refused by the RESOLVED one.
//
// This is the split that fixes the skipped floor. validateBounds answers what
// needs no row -- negative, over the ceiling -- so a partial goes through it;
// validateSize answers the size the machine will actually be given, which is
// where the floor belongs.
func TestAPartialSizePassesTheEdgeCheckAndNotTheResolvedOne(t *testing.T) {
	if err := validateBounds(0, 2048); err != nil {
		t.Errorf("the edge check refused a partial resize: %v", err)
	}
	if err := validateSize(0, 2048); err == nil {
		t.Error("the resolved check accepted a size with no vCPUs")
	}
}

// The floor applies to a partial resize once it is resolved.
//
// `--mem 32` names one field. Before the split, validateSize ran on `0, 32`,
// took the partial branch and returned nil -- so the request was accepted, the
// vCPU count was filled in from the row, and the machine was rebuilt at 32 MiB,
// which no guest can boot. The one request shape the floor exists for was the
// one shape that never reached it.
func TestAPartialResizeStillMeetsTheMemoryFloor(t *testing.T) {
	m, st := storeManager(t)
	putResizable(t, st, &state.Machine{
		ID: "m_small", HostID: "host-a", VCPUs: 2, MemMiB: 2048,
	})

	_, err := m.Resize(t.Context(), "m_small", 0, MinMemMiB-1)
	if err == nil {
		t.Fatal("a partial resize below the boot floor was accepted")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("refusal is %v, want ErrInvalid so the API answers 400", err)
	}
	if !strings.Contains(err.Error(), "boot") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
