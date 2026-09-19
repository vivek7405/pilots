package machines

import (
	"errors"
	"strings"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/api"
	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// forkVolumeManager is a suspended, volume-backed machine and the fakes around
// it. Enough to drive Fork as far as the create, which is where the volume
// work happens.
func forkVolumeManager(t *testing.T) (*Manager, *fakeVolumes) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	fv := &fakeVolumes{store: st, snapshots: map[string][]string{}}
	m := &Manager{opts: Options{
		HostID: "host-a", Store: st, Volumes: fv,
	}, flight: newInFlight()}

	if err := st.PutVolume(t.Context(), &state.Volume{
		ID: "v_parent", Name: "data", HostID: "host-a", MachineID: "m_parent",
		SizeMiB: 10240, MountPath: "/data",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutMachine(t.Context(), &state.Machine{
		ID: "m_parent", Name: "parent", HostID: "host-a", State: StateSuspended,
		VCPUs: 2, MemMiB: 2048, MemBuildID: "mem-1", RootfsBuildID: "rootfs-1",
		VolumeID: "v_parent",
	}); err != nil {
		t.Fatal(err)
	}
	return m, fv
}

// `volume: true` actually forks the volume.
//
// forkSource has carried a VolumeSnapshot field since this feature was
// written, documented as "the point the forked volume is filled from, taken at
// the same moment as the memory image so the two agree". Nothing ever set it,
// and forkOnce built its create request with no volume at all -- so the flag
// was accepted and IGNORED, and the fork came up with memory expecting a disk
// it did not have. That is word for word the failure the guard refuses when
// you do NOT ask for a volume, performed when you do.
func TestForkingWithAVolumeActuallyForksTheVolume(t *testing.T) {
	m, fv := forkVolumeManager(t)

	// The create at the end has no netns or Firecracker here, so each fork
	// carries an error. What is asserted is the volume work before it.
	_, _ = m.Fork(t.Context(), api.ForkOptions{Machine: "m_parent", Count: 2, Volume: true})

	stamps := fv.snapshots["v_parent"]
	if len(stamps) != 1 {
		t.Fatalf("the parent volume was snapshotted %d times, want once for the "+
			"whole request: every fork must begin from ONE instant", len(stamps))
	}
	if len(fv.copied) != 2 {
		t.Errorf("filled %d fork volumes, want one per fork: %v", len(fv.copied), fv.copied)
	}
	for _, c := range fv.copied {
		if !strings.Contains(c, stamps[0]) {
			t.Errorf("a fork volume was filled from %q, not from the one snapshot %q",
				c, stamps[0])
		}
	}
}

// A fork that does NOT ask for the volume is refused, and now on the
// checkpoint path too.
//
// The machine path always refused it. The checkpoint path had no such check,
// so forking a checkpoint of a volume-backed machine went the same silent way.
func TestForkingAVolumeBackedMachineWithoutTheFlagIsRefused(t *testing.T) {
	m, _ := forkVolumeManager(t)

	_, err := m.Fork(t.Context(), api.ForkOptions{Machine: "m_parent", Count: 1})
	if err == nil {
		t.Fatal("a volume-backed machine was forked without its volume")
	}
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("refusal is %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "volume: true") {
		t.Errorf("the refusal does not say what to pass: %v", err)
	}
}

// A machine with NO volume forks exactly as it did, with no volume work at all.
func TestForkingAMachineWithNoVolumeTouchesNoVolumes(t *testing.T) {
	m, fv := forkVolumeManager(t)
	if err := m.opts.Store.PutMachine(t.Context(), &state.Machine{
		ID: "m_plain", Name: "plain", HostID: "host-a", State: StateSuspended,
		VCPUs: 1, MemMiB: 512, MemBuildID: "mem-2", RootfsBuildID: "rootfs-2",
	}); err != nil {
		t.Fatal(err)
	}

	_, _ = m.Fork(t.Context(), api.ForkOptions{Machine: "m_plain", Count: 1})

	if len(fv.snapshots["v_parent"]) != 0 || len(fv.copied) != 0 {
		t.Errorf("a volumeless fork did volume work: snapshots=%v copied=%v",
			fv.snapshots, fv.copied)
	}
}
