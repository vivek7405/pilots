package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// This is the integration test that proves the whole boot path: a real
// Firecracker under the real jailer, in a real network namespace, with the
// guest reachable through its agent over the slot address.
//
// It needs root (namespaces, jailer) and the artifacts the fetch scripts
// install, so it skips cleanly without them.
func requireBootEnv(t *testing.T) (kernel, rootfs, fcBin, jailerBin string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: jailer and network namespaces")
	}
	if testing.Short() {
		t.Skip("boots a VM; skipped in -short")
	}

	kernel = envOr("PILOT_KERNEL", "/opt/pilots/kernels/vmlinux-6.1.158/vmlinux.bin")
	fcBin = envOr("PILOT_FIRECRACKER", "/opt/pilots/bin/firecracker")
	jailerBin = envOr("PILOT_JAILER", "/opt/pilots/bin/jailer")
	rootfs = envOr("PILOT_TEMPLATE_ROOTFS", repoPath(t, "scripts/rootfs/golden.ext4"))

	for name, path := range map[string]string{
		"kernel": kernel, "rootfs": rootfs, "firecracker": fcBin, "jailer": jailerBin,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("%s missing at %s: run the fetch/build scripts first", name, path)
		}
	}
	return
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func repoPath(t *testing.T, rel string) string {
	t.Helper()
	// internal/fc -> apps/hostd -> repo root
	return filepath.Join("..", "..", "..", "..", rel)
}

func TestBootRealMachine(t *testing.T) {
	kernel, rootfs, fcBin, jailerBin := requireBootEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	machineID := fmt.Sprintf("pilotstest-boot-%d", time.Now().UnixNano()%1e6)
	// High, varying slot so the test does not collide with a hostd running on
	// the same machine, which would hold the low indices.
	pool := netns.NewPool(1024, netip.MustParsePrefix("fdcd:1::/112"))
	slot, err := pool.Reserve(900+int(time.Now().UnixNano()%90), machineID)
	if err != nil {
		t.Fatalf("Reserve slot: %v", err)
	}

	mac, err := GenerateMAC()
	if err != nil {
		t.Fatalf("GenerateMAC: %v", err)
	}
	if err := netns.Setup(slot, mac, 0); err != nil {
		t.Fatalf("netns.Setup: %v", err)
	}
	t.Cleanup(func() { _ = netns.Teardown(slot) })

	stateDir := t.TempDir()

	// NOT t.TempDir(): /tmp is mounted nodev on most distributions, and the
	// jailer's device nodes would be unopenable. checkChrootBaseUsable would
	// reject it, but the test should exercise a realistic path anyway.
	chrootBase, err := os.MkdirTemp("/var/lib/pilots", "test-jailer-")
	if err != nil {
		t.Fatalf("chroot base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(chrootBase) })

	m, err := Boot(ctx, Config{
		MachineID: machineID, Slot: slot, MAC: mac,
		VCPUs: 1, MemMiB: 256,
		KernelPath: kernel, TemplateRootfs: rootfs,
		FirecrackerBin: fcBin, JailerBin: jailerBin,
		ChrootBase: chrootBase, StateDir: stateDir,
		JailUID: 0, JailGID: 0,
		Limits: Limits{PidsMax: 2048},
	})
	if err != nil {
		t.Fatalf("Boot: %v\n%s", err, tailLog(stateDir))
	}
	t.Cleanup(func() { _ = m.Kill() })

	// The serial console is the boot-readiness signal, which is exactly why
	// the jailer must not be given --daemonize.
	if err := waitForLog(m.SerialLog, "login:", 90*time.Second); err != nil {
		t.Fatalf("guest never reached a login prompt: %v\n%s", err, tailLog(stateDir))
	}

	// The agent must be reachable at the SLOT address from the host
	// namespace, which exercises the veth, the host route, and the namespace
	// DNAT all at once.
	if err := waitForAgent(slot.AgentAddr(), 60*time.Second); err != nil {
		t.Fatalf("guest agent unreachable at %s: %v\n%s", slot.AgentAddr(), err, tailLog(stateDir))
	}

	// Breadcrumbs must let a restarted hostd find this machine again.
	if err := m.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	found, err := Reconcile(filepath.Dir(stateDir))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var adopted bool
	for _, r := range found {
		if r.State.Pid == m.Cmd.Process.Pid && r.Alive {
			adopted = true
		}
	}
	if !adopted {
		t.Errorf("reconcile did not report the running machine as alive")
	}

	// Teardown must leave nothing: no process, no namespace, no chroot.
	pid := m.Cmd.Process.Pid
	if err := m.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Errorf("firecracker pid %d survived Kill", pid)
	}
	if _, err := os.Stat("/var/run/netns/" + slot.NetnsName); !os.IsNotExist(err) {
		t.Errorf("namespace survived Kill")
	}
	if _, err := ReadPid(stateDir); !os.IsNotExist(err) {
		t.Errorf("fc.pid survived Kill; reconcile would resurrect a destroyed machine")
	}
}

func waitForLog(path, needle string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && bytes.Contains(raw, []byte(needle)) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%q not seen in %s within %s", needle, path, timeout)
}

func waitForAgent(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/health")
		if err == nil {
			defer resp.Body.Close()
			var body map[string]any
			if json.NewDecoder(resp.Body).Decode(&body) == nil && body["ok"] == true {
				return nil
			}
			lastErr = fmt.Errorf("unexpected health payload")
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func tailLog(stateDir string) string {
	raw, err := os.ReadFile(filepath.Join(stateDir, "lifecycle.log"))
	if err != nil {
		return "(no serial log)"
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return "--- serial log tail ---\n" + strings.Join(lines, "\n")
}
