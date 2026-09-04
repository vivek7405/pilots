package api

import (
	"context"
	"net/http"
	"sync"

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
	collected                                      int
	volumesCreated                                 int
	// lastCreate is the request as the handler passed it down, so a test can
	// assert what the handler filled in -- the org above all, which must come
	// from the key and never from the body.
	lastCreate       CreateMachineRequest
	lastCreateVolume CreateVolumeRequest

	// The follow tests drive these from another goroutine while the handler
	// reads them, so both sides go through mu.
	mu       sync.Mutex
	logs     string
	logsErr  error
	streamed []string
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
		logs: "boot log",
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
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.logs), f.err
}

// ExecStream records the machine and answers 200. An httptest recorder cannot
// hijack, so the fake never answers 101; what the proxy does with a real
// socket is tested in internal/machines against an httptest server.
func (f *fakeManager) ExecStream(w http.ResponseWriter, _ *http.Request, machineID string) error {
	f.mu.Lock()
	f.streamed = append(f.streamed, machineID)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	_, _ = w.Write([]byte("stream"))
	return nil
}

func (f *fakeManager) LogTail(_ string, offset int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if offset >= int64(len(f.logs)) {
		return nil, nil
	}
	return []byte(f.logs[offset:]), nil
}

// appendLog is what a guest writing to its console looks like to a follow.
func (f *fakeManager) appendLog(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs += line
}

func (f *fakeManager) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsErr = err
}

// streamedMachines is the recorded list, copied under the lock.
func (f *fakeManager) streamedMachines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.streamed...)
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

func (f *fakeManager) CollectMetrics() { f.collected++ }
