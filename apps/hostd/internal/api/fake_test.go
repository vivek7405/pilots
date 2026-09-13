package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

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
	redeployed                                     int
	collected                                      int
	volumesCreated                                 int
	// lastCreate is the request as the handler passed it down, so a test can
	// assert what the handler filled in -- the org above all, which must come
	// from the key and never from the body.
	lastCreate       CreateMachineRequest
	lastCreateVolume CreateVolumeRequest
	lastRedeploy     RedeployRequest

	// The follow tests drive these from another goroutine while the handler
	// reads them, so both sides go through mu.
	mu       sync.Mutex
	logs     string
	logsErr  error
	streamed []string
	// The process surface, recorded so a test can assert WHICH process an
	// action named rather than only that the call happened.
	processes      string
	processActions []string
	processLogTail int
	// resizedTo is the size the last resize asked for, as {vcpus, mem_mib}.
	resizedTo [2]int
	// forked records what each fork request asked for, so a test can assert
	// the source and count reached the manager rather than only that the route
	// answered. forkFails makes the Nth fork fail, for the assertion that one
	// failure does not take its siblings with it.
	forked    []ForkOptions
	forkFails int
	// The volume snapshot surface: what was taken, listed and restored.
	volumeSnapshots   []string
	snapshotted       []string
	restoredSnapshots []string
	deletedSnapshots  []string
	forkedVolumes     []string
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

func (f *fakeManager) Redeploy(_ context.Context, _ string, req RedeployRequest) (*state.Machine, error) {
	f.redeployed++
	f.lastRedeploy = req
	return f.machine, f.err
}

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

// Resize records the size it was asked for, so a test can assert the request
// reached the manager rather than only that the route answered.
func (f *fakeManager) Resize(_ context.Context, _ string, vcpus, memMiB int) (*state.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizedTo = [2]int{vcpus, memMiB}
	if f.err != nil {
		return nil, f.err
	}
	out := *f.machine
	if vcpus > 0 {
		out.VCPUs = vcpus
	}
	if memMiB > 0 {
		out.MemMiB = memMiB
	}
	return &out, nil
}

func (f *fakeManager) Processes(context.Context, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.processes == "" {
		return []byte(`{"processes":[]}`), f.err
	}
	return []byte(f.processes), f.err
}

// ProcessAction records what was asked of which process, so a test can assert
// that restarting one names that one and not the machine.
func (f *fakeManager) ProcessAction(_ context.Context, machineID, name, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processActions = append(f.processActions, machineID+" "+action+" "+name)
	return f.err
}

func (f *fakeManager) ProcessLogs(_ context.Context, _, name string, tail int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processLogTail = tail
	return []byte("logs of " + name), f.err
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

func (f *fakeManager) TCPStream(http.ResponseWriter, *http.Request, string, int) error { return nil }

func (f *fakeManager) SessionsJSON(context.Context, string) ([]byte, error) { return []byte("[]"), nil }
func (f *fakeManager) AttachStream(http.ResponseWriter, *http.Request, string, string) error {
	return nil
}
func (f *fakeManager) KillSession(context.Context, string, string) error { return nil }

// The volume snapshot surface, recorded rather than performed: what the API
// tests assert is which volume was named and whether the owner-host forward
// happened, not what juicefs did.
func (f *fakeManager) SnapshotVolume(_ context.Context, volumeID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	stamp := "20260912T101500Z"
	f.volumeSnapshots = append([]string{stamp}, f.volumeSnapshots...)
	f.snapshotted = append(f.snapshotted, volumeID)
	return stamp, nil
}

func (f *fakeManager) ListVolumeSnapshots(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumeSnapshots, f.err
}

func (f *fakeManager) RestoreVolumeSnapshot(_ context.Context, volumeID, stamp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoredSnapshots = append(f.restoredSnapshots, volumeID+"@"+stamp)
	return f.err
}

// Fork answers with one machine per requested fork. forkFails makes that many
// of them fail, from the first, so a test can check that the successful ones
// still come back.
func (f *fakeManager) Fork(_ context.Context, opts ForkOptions) ([]ForkOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forked = append(f.forked, opts)
	if f.err != nil {
		return nil, f.err
	}
	count := opts.Count
	if count <= 0 {
		count = 1
	}
	out := make([]ForkOutcome, 0, count)
	for i := range count {
		if i < f.forkFails {
			out = append(out, ForkOutcome{Err: errors.New("no room for this one")})
			continue
		}
		row := *f.machine
		row.ID = fmt.Sprintf("m_fork_%d", i)
		row.Name = fmt.Sprintf("fork-%d", i)
		out = append(out, ForkOutcome{Machine: &row})
	}
	return out, nil
}

func (f *fakeManager) DeleteVolumeSnapshot(_ context.Context, volumeID, stamp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.volumeSnapshots[:0]
	for _, s := range f.volumeSnapshots {
		if s != stamp {
			kept = append(kept, s)
		}
	}
	f.volumeSnapshots = kept
	f.deletedSnapshots = append(f.deletedSnapshots, volumeID+"@"+stamp)
	return f.err
}

func (f *fakeManager) ForkVolumeSnapshot(_ context.Context, volumeID, stamp, name string) (*state.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.forkedVolumes = append(f.forkedVolumes, volumeID+"@"+stamp)
	out := *f.volume
	out.ID = "vol-fork"
	if name != "" {
		out.Name = name
	}
	return &out, nil
}

// Stats answers a fixed sample. Fixed rather than zero so a test asserting the
// shape can tell "the handler read the manager" from "the handler returned an
// empty struct", which are the same thing when every field is zero.
func (f *fakeManager) Stats(_ context.Context, id string) (*Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &Stats{
		CPUSeconds: 12.5, MemoryBytes: 64 << 20, MemoryLimitBytes: 512 << 20,
		SampledAt: time.Unix(1700000000, 0),
	}, nil
}
