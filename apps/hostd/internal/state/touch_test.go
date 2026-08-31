package state

import (
	"context"
	"testing"
)

// Touch must not disturb anything but the activity columns.
//
// A whole-row upsert here raced Suspend: read the row while it said running,
// let Suspend commit, write the stale copy back, and the machine claimed to be
// running while actually suspended -- a URL that never recovers, because every
// repair path trusts the row.
func TestTouchMachineOnlyUpdatesActivity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	original := &Machine{
		ID: "m-1", Name: "webapp", HostID: "host-a", State: "running",
		Domain: "webapp.pilotrun.app", AgentTokenHash: "deadbeef",
		AppPort: 45001, VCPUs: 2, MemMiB: 512,
		LastActivity: 1000, UpdatedAt: 1000,
	}
	if err := s.PutMachine(ctx, original); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	// Something else changes the machine's lifecycle state.
	suspended := *original
	suspended.State = "suspended"
	suspended.MemBuildID = "build-1"
	if err := s.PutMachine(ctx, &suspended); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	if err := s.TouchMachine(ctx, "m-1", 2000); err != nil {
		t.Fatalf("TouchMachine: %v", err)
	}

	got, err := s.GetMachine(ctx, "m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.State != "suspended" {
		t.Errorf("Touch resurrected the row to %q; the machine is suspended", got.State)
	}
	if got.MemBuildID != "build-1" {
		t.Errorf("Touch clobbered mem_build_id: %q", got.MemBuildID)
	}
	if got.LastActivity != 2000 || got.UpdatedAt != 2000 {
		t.Errorf("Touch did not record activity: last_activity=%d updated_at=%d",
			got.LastActivity, got.UpdatedAt)
	}
	if got.Domain != original.Domain || got.AgentTokenHash != original.AgentTokenHash {
		t.Errorf("Touch disturbed identity columns: %+v", got)
	}
}

func TestTouchMissingMachineIsNotAnError(t *testing.T) {
	// Touch is fire-and-forget from the router; a machine destroyed mid-request
	// must not produce noise.
	if err := openTest(t).TouchMachine(context.Background(), "gone", 1); err != nil {
		t.Errorf("TouchMachine on a missing row: %v", err)
	}
}

func TestDeleteCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for _, id := range []string{"ck-1", "ck-2"} {
		if err := s.PutCheckpoint(ctx, &Checkpoint{ID: id, MachineID: "m-1"}); err != nil {
			t.Fatalf("PutCheckpoint: %v", err)
		}
	}
	if err := s.DeleteCheckpoint(ctx, "ck-1"); err != nil {
		t.Fatalf("DeleteCheckpoint: %v", err)
	}
	got, err := s.ListCheckpoints(ctx, "m-1")
	if err != nil || len(got) != 1 || got[0].ID != "ck-2" {
		t.Errorf("after delete: %+v (err %v)", got, err)
	}
}
