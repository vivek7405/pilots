package main

import (
	"context"
	"errors"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// recordingStore records which rows were written back.
type recordingStore struct {
	machines []state.Machine
	volumes  []state.Volume
	cpu      map[string]state.MachineCPU

	putMachines []string
	putVolumes  []string
	putCPU      []string
	listErr     error
}

func (s *recordingStore) ListMachines(context.Context) ([]state.Machine, error) {
	return s.machines, s.listErr
}
func (s *recordingStore) PutMachine(_ context.Context, m *state.Machine, _ ...state.WriteOption) error {
	s.putMachines = append(s.putMachines, m.ID)
	return nil
}
func (s *recordingStore) ListVolumes(context.Context) ([]state.Volume, error) {
	return s.volumes, s.listErr
}
func (s *recordingStore) PutVolume(_ context.Context, v *state.Volume, _ ...state.WriteOption) error {
	s.putVolumes = append(s.putVolumes, v.ID)
	return nil
}
func (s *recordingStore) GetMachineCPU(_ context.Context, id string) (*state.MachineCPU, error) {
	row, ok := s.cpu[id]
	if !ok {
		return nil, state.ErrNotFound
	}
	return &row, nil
}
func (s *recordingStore) PutMachineCPU(_ context.Context, c *state.MachineCPU, _ ...state.WriteOption) error {
	s.putCPU = append(s.putCPU, c.ID)
	return nil
}

// Republishing must touch this host's rows and NOTHING else.
//
// The counterfactual is the whole test. Drop the `HostID != hostID` filter and
// this fixture republishes three machines instead of two -- including one
// owned by host-b. On a replicated store that is not a harmless extra write:
// full-row writes carry the owner column, merges are per column, and a write
// that re-asserts host_id wins on that column purely by being later. A host
// that re-published a peer's row on every restart would quietly claim machines
// it is not running, which is the single-writer violation the whole design
// exists to prevent and the one a CRDT merge will never report.
func TestRepublishWritesOnlyThisHostsRows(t *testing.T) {
	store := &recordingStore{
		machines: []state.Machine{
			{ID: "m-1", HostID: "host-a", State: "running"},
			{ID: "m-2", HostID: "host-b", State: "running"},
			{ID: "m-3", HostID: "host-a", State: "suspended"},
		},
		volumes: []state.Volume{
			{ID: "vol-1", HostID: "host-a"},
			{ID: "vol-2", HostID: "host-b"},
		},
		// m-3 has never started since machine_cpu existed, so there is nothing
		// of its to re-send. m-2's row belongs to host-b and must not move.
		cpu: map[string]state.MachineCPU{
			"m-1": {ID: "m-1", Kind: state.KindMachine, Vendor: "AuthenticAMD"},
			"m-2": {ID: "m-2", Kind: state.KindMachine, Vendor: "GenuineIntel"},
		},
	}

	n, err := republishOwnRows(context.Background(), "host-a", store)
	if err != nil {
		t.Fatalf("republishOwnRows: %v", err)
	}

	wantMachines := []string{"m-1", "m-3"}
	if !equal(store.putMachines, wantMachines) {
		t.Errorf("republished machines %v, want %v -- a row belonging to another "+
			"host was written back", store.putMachines, wantMachines)
	}
	// A vendor write that never left this host leaves every peer ranking the
	// machine into the wrong pool, which surfaces as a needless cold boot.
	wantCPU := []string{"m-1"}
	if !equal(store.putCPU, wantCPU) {
		t.Errorf("republished cpu rows %v, want %v", store.putCPU, wantCPU)
	}
	wantVolumes := []string{"vol-1"}
	if !equal(store.putVolumes, wantVolumes) {
		t.Errorf("republished volumes %v, want %v", store.putVolumes, wantVolumes)
	}
	if n != 4 {
		t.Errorf("reported %d rows, want 4 (two machines, one cpu row and one volume)", n)
	}
}

// A host that owns nothing must write nothing, not "everything it can see".
func TestRepublishWritesNothingWhenThisHostOwnsNothing(t *testing.T) {
	store := &recordingStore{
		machines: []state.Machine{{ID: "m-1", HostID: "host-b"}},
		volumes:  []state.Volume{{ID: "vol-1", HostID: "host-b"}},
		cpu: map[string]state.MachineCPU{
			"m-1": {ID: "m-1", Kind: state.KindMachine, Vendor: "GenuineIntel"},
		},
	}
	n, err := republishOwnRows(context.Background(), "host-a", store)
	if err != nil {
		t.Fatalf("republishOwnRows: %v", err)
	}
	if n != 0 || len(store.putMachines) != 0 || len(store.putVolumes) != 0 || len(store.putCPU) != 0 {
		t.Fatalf("wrote %d rows (machines %v, cpu %v, volumes %v) while owning none",
			n, store.putMachines, store.putCPU, store.putVolumes)
	}
}

// A replica that cannot be read is reported, never treated as "no rows".
// Silently succeeding here would mean a host whose replica is broken looks
// like a host with nothing to republish.
func TestRepublishReportsAnUnreadableReplica(t *testing.T) {
	store := &recordingStore{listErr: errors.New("replica unavailable")}
	if _, err := republishOwnRows(context.Background(), "host-a", store); err == nil {
		t.Fatal("a failed list was reported as success")
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
