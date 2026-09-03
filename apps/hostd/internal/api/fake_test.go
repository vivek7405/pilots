package api

import (
	"context"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeManager stands in for the lifecycle layer so the API's routing, auth and
// encoding can be tested without booting a machine.
type fakeManager struct {
	machine    *state.Machine
	checkpoint *state.Checkpoint
	volume     *state.Volume
	err        error

	created, destroyed, suspended, woken, restored int
	volumesCreated                                 int
	// lastCreate is the request as the handler passed it down, so a test can
	// assert what the handler filled in -- the org above all, which must come
	// from the key and never from the body.
	lastCreate       CreateMachineRequest
	lastCreateVolume CreateVolumeRequest
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		machine: &state.Machine{
			ID: "m_1", Name: "webapp", HostID: "host-test", State: "running",
			Domain: "webapp.pilotrun.app", VCPUs: 1, MemMiB: 512,
		},
		checkpoint: &state.Checkpoint{ID: "ck_1", MachineID: "m_1", Seq: 1},
		volume: &state.Volume{
			ID: "vol-1", Name: "data", SizeMiB: 10240, HostID: "host-test",
			MountPath: "/data",
		},
	}
}

func (f *fakeManager) Create(_ context.Context, req CreateMachineRequest) (*state.Machine, error) {
	f.created++
	f.lastCreate = req
	return f.machine, f.err
}
func (f *fakeManager) Destroy(context.Context, string) error { f.destroyed++; return f.err }
func (f *fakeManager) Suspend(context.Context, string) error { f.suspended++; return f.err }
func (f *fakeManager) Wake(context.Context, string) error    { f.woken++; return f.err }

func (f *fakeManager) Checkpoint(context.Context, string, string) (*state.Checkpoint, error) {
	return f.checkpoint, f.err
}
func (f *fakeManager) ListCheckpoints(context.Context, string) ([]state.Checkpoint, error) {
	return []state.Checkpoint{*f.checkpoint}, f.err
}
func (f *fakeManager) RestoreCheckpoint(context.Context, string) (*state.Machine, error) {
	f.restored++
	return f.machine, f.err
}
func (f *fakeManager) GetCheckpoint(context.Context, string) (*state.Checkpoint, error) {
	return f.checkpoint, f.err
}
func (f *fakeManager) Exec(context.Context, string, ExecRequest) (*ExecResponse, error) {
	return &ExecResponse{Stdout: "hello\n", ExitCode: 0}, f.err
}
func (f *fakeManager) Logs(context.Context, string) ([]byte, error) {
	return []byte("boot log"), f.err
}
func (f *fakeManager) CreateVolume(_ context.Context, req CreateVolumeRequest) (*state.Volume, error) {
	f.volumesCreated++
	f.lastCreateVolume = req
	return f.volume, f.err
}
func (f *fakeManager) ListVolumes(context.Context) ([]state.Volume, error) {
	return []state.Volume{*f.volume}, f.err
}
func (f *fakeManager) MachineVolume(context.Context, string) (*MachineVolume, error) {
	return &MachineVolume{
		VolumeID: f.volume.ID, MountPath: f.volume.MountPath,
		Device: "/dev/vdb", CacheType: "Writeback",
	}, f.err
}
