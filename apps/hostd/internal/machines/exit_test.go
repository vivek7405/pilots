package machines

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Nothing here can bring a guest up, so what is asserted is what survives
// without one: the row, the order of the writes and the deletes, the registry,
// the slot, and the line in the machine's log. That is the whole permanent
// half of the reaction, and it is the half a mistake in would be silent.
//
// The counterfactual for every test in this file is the incident: the row
// stayed at running forever, the router kept proxying to a guest that did not
// exist, and the idle monitor retried a suspend against the corpse every ten
// seconds until somebody deleted the machine by hand.

// orderedUploader records its deletes into the SAME log as the store's writes,
// so "the row named the new builds before anything was removed" is one
// comparison rather than two clocks.
type orderedUploader struct {
	*deletingUploader
	rec *recordingStore
}

func (u *orderedUploader) Delete(ctx context.Context, key string) error {
	u.rec.note("Delete:" + key)
	return u.deletingUploader.Delete(ctx, key)
}

func newExitManager(t *testing.T) (*Manager, *recordingStore, *orderedUploader) {
	t.Helper()

	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rec := &recordingStore{Store: st}
	up := &orderedUploader{deletingUploader: &deletingUploader{}, rec: rec}
	m := New(Options{
		HostID: "host-a", Store: rec, Vendor: "AuthenticAMD",
		Uploader: up, Chunks: up, CacheRoot: t.TempDir(), StateRoot: t.TempDir(),
	})
	return m, rec, up
}

// runningRow is a machine this host runs, with a template it can never fetch
// so every path that needs one fails fast instead of photographing a new one.
func runningRow(id string) *state.Machine {
	return &state.Machine{
		ID: id, Name: id, HostID: "host-a", State: StateRunning,
		Domain: id + ".pilotrun.app", VCPUs: 1, MemMiB: 512,
		TemplateMemBuildID:    uuid.NewString(),
		TemplateRootfsBuildID: uuid.NewString(),
	}
}

// exitMachine builds the handle for a machine whose "Firecracker" is a sleep,
// so the test can kill it and watch what hostd does about it.
func exitMachine(t *testing.T, m *Manager, id string) *fc.Machine {
	t.Helper()

	dir := m.stateDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Out of the registry BEFORE the kill, so tearing the test down does not
	// drive a reaction against a store that is already closed.
	t.Cleanup(func() {
		m.drop(id)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})

	return &fc.Machine{
		ID: id, StateDir: dir, SerialLog: filepath.Join(dir, "lifecycle.log"),
		Cmd: cmd,
	}
}

// waitForWrites blocks until the store has recorded n machine-row writes.
func waitForWrites(t *testing.T, rec *recordingStore, n int) []string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		order := rec.order()
		if countWrites(order) >= n {
			return order
		}
		if time.Now().After(deadline) {
			t.Fatalf("the exit produced %d row writes in 5s, want %d: %v",
				countWrites(order), n, order)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func countWrites(order []string) int {
	n := 0
	for _, call := range order {
		if strings.HasPrefix(call, "PutMachine:") {
			n++
		}
	}
	return n
}

func writesOnly(order []string) []string {
	var out []string
	for _, call := range order {
		if strings.HasPrefix(call, "PutMachine:") {
			out = append(out, call)
		}
	}
	return out
}

// A replica the rollout can replace is marked error and left alone.
//
// The autoscaler counts only running replicas toward the floor and wakes only
// suspended or stopped ones, so an error replica is absent by construction and
// a replacement is restored from the release on the next tick. That is a
// sub-second restore from a gated image; cold-booting the crashed one would be
// a kernel boot.
func TestAnUnexpectedExitMarksAReplicaErrorAndLeavesItToTheAutoscaler(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	row := runningRow("m-replica")
	row.ServiceID, row.ReleaseID, row.Slot = "svc-1", "rel-1", 7
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	fcm := exitMachine(t, m, "m-replica")
	m.put("m-replica", fcm)
	if err := syscall.Kill(fcm.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	order := waitForWrites(t, rec, 1)

	got, err := m.opts.Store.GetMachine(ctx, "m-replica")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateError {
		t.Errorf("the row says %q after its firecracker exited, want error", got.State)
	}
	if got.Slot != 7 {
		t.Errorf("the replica's slot index is %d, want 7 kept while it is down", got.Slot)
	}
	if _, ok := m.get("m-replica"); ok {
		t.Error("the registry still holds a machine whose process is gone")
	}
	// Exactly one write: nothing restarted it in place.
	if w := writesOnly(order); len(w) != 1 || w[0] != "PutMachine:error" {
		t.Errorf("row writes were %v, want exactly one PutMachine:error", w)
	}
	log, err := os.ReadFile(fcm.SerialLog)
	if err != nil {
		t.Fatalf("read the lifecycle log: %v", err)
	}
	if !strings.Contains(string(log), "exited on its own") {
		t.Errorf("the lifecycle log does not record the exit: %q", log)
	}
}

// A sandbox has no release to be replaced from, so it is brought back in
// place. The restart is Wake, which fails here for want of a real image and
// writes error a second time -- the second write is what proves something
// tried.
func TestAnExitedSandboxIsBroughtBackOnce(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	row := runningRow("m-sandbox")
	row.MemBuildID = uuid.NewString()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	fcm := exitMachine(t, m, "m-sandbox")
	m.put("m-sandbox", fcm)
	if err := syscall.Kill(fcm.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	order := waitForWrites(t, rec, 2)

	if w := writesOnly(order); len(w) != 2 {
		t.Errorf("row writes were %v, want the settle write and the restart's", w)
	}
	got, err := m.opts.Store.GetMachine(ctx, "m-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slot != 0 {
		t.Errorf("a sandbox kept slot %d while it was down", got.Slot)
	}
	if _, ok := m.exits.Load("m-sandbox"); !ok {
		t.Error("the exit was not remembered, so a crash loop would restart forever")
	}
}

// A machine that dies twice inside a minute is a crash loop, and restarting it
// again would be a loop hostd drives rather than one it stops.
func TestASecondExitInsideAMinuteIsNotRestarted(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	row := runningRow("m-loop")
	row.MemBuildID = uuid.NewString()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	m.exits.Store("m-loop", time.Now())
	rec.calls = nil

	fcm := exitMachine(t, m, "m-loop")
	m.put("m-loop", fcm)
	if err := syscall.Kill(fcm.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, rec, 1)

	// Long enough for a restart to have written its own row if one had run.
	time.Sleep(500 * time.Millisecond)
	if w := writesOnly(rec.order()); len(w) != 1 {
		t.Errorf("row writes were %v, want exactly one: a second exit inside a "+
			"minute must not be restarted", w)
	}
}

// A replica holding a volume cannot be replaced: no second machine can mount
// the volume beside it, which is why the autoscaler refuses to create one. So
// it comes back in place, like a sandbox.
func TestAnExitedVolumeReplicaIsRestartedInPlace(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	row := runningRow("m-vol")
	row.ServiceID, row.ReleaseID, row.VolumeID = "svc-1", "rel-1", "vol-1"
	row.MemBuildID = uuid.NewString()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	fcm := exitMachine(t, m, "m-vol")
	m.put("m-vol", fcm)
	if err := syscall.Kill(fcm.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	order := waitForWrites(t, rec, 2)

	if w := writesOnly(order); len(w) != 2 {
		t.Errorf("row writes were %v, want the settle write and the restart's", w)
	}
}

// An exit hostd ASKED for is not an exit it reacts to. Without the flag every
// destroy and every suspend would flip its own row to error and the host would
// bring back what it just took down.
func TestAKilledMachineIsNotAnExit(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachine(ctx, runningRow("m-killed")); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	fcm := exitMachine(t, m, "m-killed")
	m.put("m-killed", fcm)
	if err := fcm.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	if w := writesOnly(rec.order()); len(w) != 0 {
		t.Errorf("a deliberate kill wrote %v", w)
	}
	got, err := m.opts.Store.GetMachine(ctx, "m-killed")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning {
		t.Errorf("a deliberate kill moved the row to %q", got.State)
	}
}

// The exit of a process the registry has replaced belongs to nobody. A
// redeploy's old process must not mark the new one's row.
func TestAStaleExitIsIgnored(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachine(ctx, runningRow("m-stale")); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	old := exitMachine(t, m, "m-stale")
	m.put("m-stale", old)
	m.drop("m-stale")
	fresh := exitMachine(t, m, "m-stale")
	m.put("m-stale", fresh)

	if err := syscall.Kill(old.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	if w := writesOnly(rec.order()); len(w) != 0 {
		t.Errorf("a superseded process's exit wrote %v", w)
	}
	if cur, ok := m.get("m-stale"); !ok || cur != fresh {
		t.Error("the stale exit dropped the machine that replaced it")
	}
}

// The disk the block server still holds is the freshest durable state the
// machine has, so it is captured on the way out and the memory image goes with
// it: that image describes a disk this one has moved past, and pairing them on
// the next wake is the disagreement `sync` before every snapshot exists to
// prevent.
//
// The deletes run AFTER the row names the new build, so nothing can read a row
// naming a build that is already gone.
func TestACapturedDiskDropsTheMemoryImage(t *testing.T) {
	m, rec, up := newExitManager(t)
	ctx := context.Background()

	tpl := stageTemplate(t, m)
	memOld, rootfsOld := uuid.NewString(), uuid.NewString()

	row := runningRow("m-capture")
	row.MemBuildID, row.RootfsBuildID = memOld, rootfsOld
	row.TemplateMemBuildID = tpl.MemBuildID.String()
	row.TemplateRootfsBuildID = tpl.RootfsBuildID.String()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	fcm := exitMachine(t, m, "m-capture")
	// The jail's disk file, which is what a machine with no block server
	// leaves behind: a full copy of the template with this machine's writes in
	// it.
	fcm.ChrootDir = filepath.Join(t.TempDir(), "root")
	stageJailDisk(t, fcm.ChrootDir)

	m.put("m-capture", fcm)
	if err := syscall.Kill(fcm.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, rec, 1)

	got, err := m.opts.Store.GetMachine(ctx, "m-capture")
	if err != nil {
		t.Fatal(err)
	}
	if got.MemBuildID != "" {
		t.Errorf("the memory image survived a captured disk: %q", got.MemBuildID)
	}
	if got.RootfsBuildID == "" || got.RootfsBuildID == rootfsOld {
		t.Fatalf("the row's disk build is %q, want the one just captured", got.RootfsBuildID)
	}

	want := map[string]bool{
		memOld + "/header":          true,
		memOld + "/data":            true,
		rootfsOld + "/header":       true,
		rootfsOld + "/data":         true,
		suspendSnapKey("m-capture"): true,
		prefetchKey("m-capture"):    true,
	}
	deleted := up.keys()
	for key := range want {
		if !contains(deleted, key) {
			t.Errorf("%q was not discarded; it is superseded and nothing references it", key)
		}
	}

	// Every delete after the row write, never before.
	order := rec.order()
	writeAt := -1
	for i, call := range order {
		if call == "PutMachine:error" {
			writeAt = i
			break
		}
	}
	if writeAt < 0 {
		t.Fatalf("no row write in %v", order)
	}
	for i, call := range order[:writeAt] {
		if strings.HasPrefix(call, "Delete:") {
			t.Errorf("call %d (%s) removed an object before the row was written", i, call)
		}
	}
}

func contains(all []string, want string) bool {
	for _, got := range all {
		if got == want {
			return true
		}
	}
	return false
}

// stageTemplate lays down a real golden template on this manager's cache: a
// chunkified rootfs build plus the manifest that names it, so templateFor
// answers from disk rather than photographing one.
func stageTemplate(t *testing.T, m *Manager) *Template {
	t.Helper()

	src := filepath.Join(t.TempDir(), "template.ext4")
	if err := os.WriteFile(src, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	tpl := &Template{
		MemBuildID: uuid.New(), RootfsBuildID: uuid.New(),
		PageSizeKiB: m.pageSizeKiB(), CreatedAt: time.Now().Unix(),
	}
	for _, id := range []uuid.UUID{tpl.MemBuildID, tpl.RootfsBuildID} {
		if _, _, err := block.Chunkify(context.Background(), block.ChunkifyOpts{
			In:      src,
			OutDir:  filepath.Join(m.buildDir(), id.String()),
			BuildID: id,
		}); err != nil {
			t.Fatalf("stage template build: %v", err)
		}
	}
	if err := m.saveTemplate(tpl); err != nil {
		t.Fatal(err)
	}
	return tpl
}

// stageJailDisk writes the machine's own copy of the template disk into its
// jail, with a block changed so the capture has something to store.
func stageJailDisk(t *testing.T, chrootDir string) {
	t.Helper()

	path := filepath.Join(chrootDir, fc.BakedRootfsPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	disk := make([]byte, 1<<20)
	copy(disk[4096:], []byte("this machine wrote here"))
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A process that died while hostd was down is an exit nobody handled, and
// reconcile is the only place it can still be noticed.
//
// The other half is hard rule 1: a row this host does not own is not this
// host's to write, however much wreckage it left behind here.
func TestExitedWhileDownReactsWhenTheRowIsOurs(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachine(ctx, runningRow("m-down")); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	m.ExitedWhileDown(ctx, fc.State{MachineID: "m-down", Pid: 999999})

	got, err := m.opts.Store.GetMachine(ctx, "m-down")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateError {
		t.Errorf("a machine that died while hostd was down says %q, want error", got.State)
	}

	foreign := runningRow("m-theirs")
	foreign.HostID = "host-b"
	if err := m.opts.Store.PutMachine(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	m.ExitedWhileDown(ctx, fc.State{MachineID: "m-theirs", Pid: 999998})

	if w := writesOnly(rec.order()); len(w) != 0 {
		t.Errorf("another host's row was written: %v", w)
	}
	if _, ok := m.get("m-theirs"); ok {
		t.Error("a foreign machine was left in this host's registry")
	}
}

// A registry entry with no process behind it is not an exit.
//
// StopLocal and the volume teardown both build a handle that carries only what
// Cleanup needs, with no Cmd at all. Reading that as "the process is gone"
// dropped the machine out of the registry underneath the caller that had just
// put it there, and the volume was then never released -- a mount left holding
// the metadata database of a machine another host now owns.
func TestAMachineWithNoProcessIsNotAnExit(t *testing.T) {
	m, rec, _ := newExitManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachine(ctx, runningRow("m-handle")); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	m.put("m-handle", &fc.Machine{ID: "m-handle"})

	time.Sleep(500 * time.Millisecond)
	if _, ok := m.get("m-handle"); !ok {
		t.Fatal("a handle with no process was dropped from the registry as if it had exited")
	}
	if w := writesOnly(rec.order()); len(w) != 0 {
		t.Errorf("a handle with no process wrote %v", w)
	}
}
