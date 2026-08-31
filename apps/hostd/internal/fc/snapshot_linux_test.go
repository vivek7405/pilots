package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// localStore is an Uploader backed by a directory.
//
// Standing in for object storage keeps this test about the snapshot mechanics
// -- what Firecracker writes, what survives, what has to be re-staged -- rather
// than about S3 itself. The interface is the same one the real client
// satisfies, so the code under test is unchanged.
type localStore struct{ root string }

func (l *localStore) PutFile(_ context.Context, key, path string) error {
	dst := filepath.Join(l.root, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyFile(path, dst)
}

func (l *localStore) GetToFile(_ context.Context, key, path string) error {
	src := filepath.Join(l.root, key)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("not found: %s", key)
	}
	return copyFile(src, path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// bootTestMachine brings up a real machine ready for snapshot work.
func bootTestMachine(t *testing.T, name string) (*Machine, Config, *netns.Pool) {
	t.Helper()
	kernel, rootfs, fcBin, jailerBin := requireBootEnv(t)

	machineID := fmt.Sprintf("pilotstest-%s-%d", name, time.Now().UnixNano()%1e6)
	pool := netns.NewPool(1024)
	slot, err := pool.Take(machineID)
	if err != nil {
		t.Fatalf("Take slot: %v", err)
	}
	mac, err := GenerateMAC()
	if err != nil {
		t.Fatalf("GenerateMAC: %v", err)
	}
	if err := netns.Setup(slot, mac, 0); err != nil {
		t.Fatalf("netns.Setup: %v", err)
	}
	t.Cleanup(func() { _ = netns.Teardown(slot) })

	chrootBase, err := os.MkdirTemp("/var/lib/pilots", "test-jailer-")
	if err != nil {
		t.Fatalf("chroot base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(chrootBase) })

	cfg := Config{
		MachineID: machineID, Slot: slot, MAC: mac,
		VCPUs: 1, MemMiB: 256,
		KernelPath: kernel, TemplateRootfs: rootfs,
		FirecrackerBin: fcBin, JailerBin: jailerBin,
		ChrootBase: chrootBase, StateDir: t.TempDir(),
		JailUID: 0, JailGID: 0, Limits: Limits{PidsMax: 2048},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	m, err := Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("Boot: %v\n%s", err, tailLog(cfg.StateDir))
	}
	t.Cleanup(func() { _ = m.Kill() })

	if err := waitForAgent(slot.AgentAddr(), 90*time.Second); err != nil {
		t.Fatalf("agent unreachable: %v\n%s", err, tailLog(cfg.StateDir))
	}
	return m, cfg, pool
}

// guestExec runs a command inside the machine through its agent.
func guestExec(t *testing.T, slot *netns.Slot, cmd string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "user": "root"})

	req, err := http.NewRequest(http.MethodPost,
		"http://"+slot.AgentAddr()+"/exec", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build exec: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer placeholder-replaced-at-create")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("exec %q: %v", cmd, err)
	}
	defer resp.Body.Close()

	var out struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode exec response: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exec %q exited %d: %s", cmd, out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(out.Stdout)
}

// The core scale-to-zero proof: a machine's disk AND its running processes
// survive being suspended to object storage and restored.
func TestSuspendAndRestoreRoundTrip(t *testing.T) {
	m, cfg, _ := bootTestMachine(t, "suspend")
	ctx := context.Background()

	// Write a marker to disk and start a process whose state only exists in
	// memory, so the restore has to bring back both.
	guestExec(t, cfg.Slot, "echo persisted-marker > /root/marker.txt")
	guestExec(t, cfg.Slot, "nohup sleep 9999 >/dev/null 2>&1 & echo started")
	before := guestExec(t, cfg.Slot, "pgrep -f 'sleep 9999' | head -1")
	if before == "" {
		t.Fatal("the in-memory process never started")
	}

	store := &localStore{root: t.TempDir()}
	at := Artifacts{Prefix: "machines/" + cfg.MachineID + "/suspend"}

	res, err := m.Suspend(ctx, store, at)
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if !res.RootfsSaved {
		t.Error("the machine wrote to disk, so its rootfs should have been saved")
	}
	for _, key := range []string{at.Snap(), at.Mem(), at.Rootfs()} {
		if _, err := os.Stat(filepath.Join(store.root, key)); err != nil {
			t.Errorf("%s missing from the store: %v", key, err)
		}
	}

	// Suspend released the namespace along with the process; Restore rebuilds
	// it, which is the same path a restore onto another host takes.
	restored, err := Restore(ctx, RestoreConfig{
		Config: cfg, Artifacts: at, LocalDir: t.TempDir(),
		AgentToken: "placeholder-replaced-at-create",
	}, store)
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, tailLog(cfg.StateDir))
	}
	t.Cleanup(func() { _ = restored.Kill() })

	if err := waitForAgent(cfg.Slot.AgentAddr(), 60*time.Second); err != nil {
		t.Fatalf("agent unreachable after restore: %v\n%s", err, tailLog(cfg.StateDir))
	}

	if got := guestExec(t, cfg.Slot, "cat /root/marker.txt"); got != "persisted-marker" {
		t.Errorf("disk did not survive the round trip: marker = %q", got)
	}
	// The same pid still running proves memory was restored, not rebooted.
	if after := guestExec(t, cfg.Slot, "pgrep -f 'sleep 9999' | head -1"); after != before {
		t.Errorf("in-memory process did not survive: pid was %q, now %q", before, after)
	}

	// The clock poke must have corrected CLOCK_REALTIME; a frozen clock is the
	// silent failure this guards.
	guestEpoch := guestExec(t, cfg.Slot, "date +%s")
	var guestSec int64
	fmt.Sscanf(guestEpoch, "%d", &guestSec)
	if drift := time.Now().Unix() - guestSec; drift > 30 || drift < -30 {
		t.Errorf("guest wall clock is %ds from the host's; the clock poke did not land", drift)
	}
}

// A checkpoint must leave the machine RUNNING -- that is the difference
// between it and a suspend, and what makes per-message checkpointing usable.
func TestCheckpointResumesGuestImmediately(t *testing.T) {
	m, cfg, _ := bootTestMachine(t, "ckpt")
	ctx := context.Background()

	guestExec(t, cfg.Slot, "echo v1 > /root/state.txt")

	store := &localStore{root: t.TempDir()}
	at := Artifacts{Prefix: "machines/" + cfg.MachineID + "/checkpoints/v1"}
	localDir := filepath.Join(t.TempDir(), "v1")

	start := time.Now()
	if err := m.Checkpoint(ctx, store, at, localDir); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	elapsed := time.Since(start)

	// The guest must be serving again immediately, without waiting for any
	// upload.
	if got := guestExec(t, cfg.Slot, "cat /root/state.txt"); got != "v1" {
		t.Errorf("machine not usable right after checkpoint: %q", got)
	}
	t.Logf("checkpoint returned in %s", elapsed)

	// Durability is reported separately, and arrives after the call returns.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if st := StatusOf(localDir); st.Durable {
			break
		} else if st.Failed {
			t.Fatalf("checkpoint upload failed: %s", st.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	st := StatusOf(localDir)
	if !st.Durable {
		t.Error("checkpoint never became durable")
	}
	if !st.Present {
		t.Error("local copy should still be present on the host that took it")
	}
}

// Restoring an older checkpoint must discard everything written after it.
func TestCheckpointRestoreRollsBackDisk(t *testing.T) {
	m, cfg, _ := bootTestMachine(t, "rollback")
	ctx := context.Background()

	guestExec(t, cfg.Slot, "echo v1 > /root/state.txt")

	store := &localStore{root: t.TempDir()}
	at := Artifacts{Prefix: "machines/" + cfg.MachineID + "/checkpoints/v1"}
	localDir := filepath.Join(t.TempDir(), "v1")
	if err := m.Checkpoint(ctx, store, at, localDir); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Mutate after the checkpoint; this must not survive the restore.
	guestExec(t, cfg.Slot, "echo v2 > /root/state.txt")
	guestExec(t, cfg.Slot, "touch /root/created-after-checkpoint")
	if got := guestExec(t, cfg.Slot, "cat /root/state.txt"); got != "v2" {
		t.Fatalf("setup failed: state is %q, want v2", got)
	}

	if err := m.Kill(); err != nil {
		t.Fatalf("Kill before restore: %v", err)
	}

	restored, err := Restore(ctx, RestoreConfig{
		Config: cfg, Artifacts: at, LocalDir: t.TempDir(),
		AgentToken: "placeholder-replaced-at-create",
	}, store)
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, tailLog(cfg.StateDir))
	}
	t.Cleanup(func() { _ = restored.Kill() })

	if err := waitForAgent(cfg.Slot.AgentAddr(), 60*time.Second); err != nil {
		t.Fatalf("agent unreachable after restore: %v\n%s", err, tailLog(cfg.StateDir))
	}

	if got := guestExec(t, cfg.Slot, "cat /root/state.txt"); got != "v1" {
		t.Errorf("rollback failed: state is %q, want v1", got)
	}
	if got := guestExec(t, cfg.Slot,
		"test -e /root/created-after-checkpoint && echo present || echo absent"); got != "absent" {
		t.Errorf("a file created after the checkpoint survived the rollback")
	}
}
