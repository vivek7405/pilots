package fc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// initRetryInterval and initDeadline bound the clock-correction poke.
//
// Aggressive on purpose: until it lands the guest's wall clock is stuck at the
// instant of the snapshot, and the failure mode is silent -- the guest accepts
// TCP connections and never services them, and TLS fails on certificate
// validity windows.
const (
	initRetryInterval = 5 * time.Millisecond
	initDeadline      = 15 * time.Second
)

// RestoreConfig describes a restore.
//
// It is deliberately the same shape for waking a suspended machine, rolling
// back to a checkpoint, and (from Phase 3) creating from a template: all three
// are one operation with different artifacts.
type RestoreConfig struct {
	Config               // the machine's shape and binaries
	Artifacts  Artifacts // where to restore from
	LocalDir   string    // local cache for the snapshot files
	AgentToken string    // for the post-resume clock poke
}

// Restore brings a machine back from object storage.
//
// The order matters. The namespace must exist before Firecracker starts,
// because the jailer joins it at exec time -- so the fetch is what overlaps
// with namespace setup, not the process spawn.
func Restore(ctx context.Context, cfg RestoreConfig, dl Uploader) (*Machine, error) {
	if err := os.MkdirAll(cfg.LocalDir, 0o755); err != nil {
		return nil, fmt.Errorf("fc: mkdir restore dir: %w", err)
	}

	localSnap := filepath.Join(cfg.LocalDir, SnapFile)
	localMem := filepath.Join(cfg.LocalDir, MemFile)
	localRootfs := filepath.Join(cfg.LocalDir, RootfsFile)

	// Fetch only what is missing. On the host that took the snapshot these are
	// already here; on any OTHER host nothing is, which is exactly the
	// cross-host restore path.
	if err := fetchIfAbsent(ctx, dl, cfg.Artifacts.Snap(), localSnap); err != nil {
		return nil, err
	}
	if err := fetchIfAbsent(ctx, dl, cfg.Artifacts.Mem(), localMem); err != nil {
		return nil, err
	}
	rootfsErr := fetchIfAbsent(ctx, dl, cfg.Artifacts.Rootfs(), localRootfs)
	haveRootfs := rootfsErr == nil
	if rootfsErr != nil && !isMissing(rootfsErr) {
		return nil, rootfsErr
	}

	// Build the network namespace. Restore owns this rather than assuming it
	// survives, because in the two cases that matter it does not: a suspend
	// releases the namespace so a stopped machine holds no host resources, and
	// a restore onto a DIFFERENT host has never had one. Setup is
	// teardown-first, so re-creating an existing namespace is safe.
	//
	// This must complete before the jailer starts -- it joins the namespace at
	// exec time -- so it is the object-storage fetch above, not the process
	// spawn, that overlaps with it.
	if err := netns.Setup(cfg.Slot, cfg.MAC, cfg.JailUID); err != nil {
		return nil, fmt.Errorf("fc: netns for restore: %w", err)
	}

	chrootDir, err := prepareRestoreJail(cfg, localSnap, localMem, localRootfs, haveRootfs)
	if err != nil {
		return nil, err
	}

	serialLog := filepath.Join(cfg.StateDir, "lifecycle.log")
	logFile, err := os.OpenFile(serialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fc: open serial log: %w", err)
	}
	defer logFile.Close()

	args := jailerArgs(cfg.Config)
	cmd := exec.CommandContext(context.Background(), cfg.JailerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fc: start jailer for restore: %w", err)
	}

	m := &Machine{
		ID:        cfg.MachineID,
		Slot:      cfg.Slot,
		Cmd:       cmd,
		Client:    NewClient(filepath.Join(chrootDir, "run", "fc.sock")),
		ChrootDir: chrootDir,
		StateDir:  cfg.StateDir,
		SerialLog: serialLog,
		StartedAt: time.Now(),
	}

	// More generous than a cold boot's wait: a restore may have just pulled
	// hundreds of megabytes from object storage.
	if err := m.Client.WaitForSocket(ctx, 60*time.Second); err != nil {
		_ = m.Kill()
		return nil, err
	}

	// Load with the VM left paused, then resume explicitly. No settle delay is
	// needed between the two.
	if err := m.Client.LoadSnapshot(ctx, SnapshotLoad{
		SnapshotPath: "/" + SnapFile,
		MemBackend:   MemBackend{BackendType: "File", BackendPath: "/" + MemFile},
		ResumeVM:     false,
		// Phase 3 turns this on for differential snapshots.
		EnableDiffSnapshots: false,
	}); err != nil {
		_ = m.Kill()
		return nil, fmt.Errorf("fc: load snapshot %s: %w", cfg.MachineID, err)
	}

	if err := m.Client.Resume(ctx); err != nil {
		_ = m.Kill()
		return nil, fmt.Errorf("fc: resume %s: %w", cfg.MachineID, err)
	}

	// Correct the guest's clock in the background: it must not delay the
	// restore, but the machine is subtly broken until it lands.
	go pokeGuestClock(cfg.Slot, cfg.AgentToken, m.ID)

	return m, nil
}

// prepareRestoreJail stages the snapshot into a fresh chroot.
func prepareRestoreJail(cfg RestoreConfig, snap, mem, rootfs string, haveRootfs bool) (string, error) {
	if err := checkChrootBaseUsable(cfg.ChrootBase); err != nil {
		return "", err
	}
	chrootDir := ChrootDir(cfg.ChrootBase, cfg.FirecrackerBin, cfg.MachineID)

	for _, dir := range []string{
		chrootDir,
		filepath.Join(chrootDir, filepath.Dir(BakedRootfsPath)),
		filepath.Join(chrootDir, "run"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("fc: mkdir %s: %w", dir, err)
		}
	}

	if err := reflinkCopy(snap, filepath.Join(chrootDir, SnapFile)); err != nil {
		return "", err
	}
	if err := reflinkCopy(mem, filepath.Join(chrootDir, MemFile)); err != nil {
		return "", err
	}

	// A machine that never wrote to disk has no saved rootfs; it restores
	// against a fresh copy of the template.
	src := rootfs
	if !haveRootfs {
		src = cfg.TemplateRootfs
	}
	jailRootfs := filepath.Join(chrootDir, BakedRootfsPath)
	if err := reflinkCopy(src, jailRootfs); err != nil {
		return "", err
	}

	for _, p := range []string{chrootDir, jailRootfs,
		filepath.Join(chrootDir, "run"),
		filepath.Join(chrootDir, SnapFile),
		filepath.Join(chrootDir, MemFile)} {
		if err := os.Chown(p, cfg.JailUID, cfg.JailGID); err != nil {
			return "", fmt.Errorf("fc: chown %s: %w", p, err)
		}
	}
	return chrootDir, nil
}

func jailerArgs(cfg Config) []string {
	args := []string{
		"--id", cfg.MachineID,
		"--exec-file", cfg.FirecrackerBin,
		"--uid", strconv.Itoa(cfg.JailUID),
		"--gid", strconv.Itoa(cfg.JailGID),
		"--chroot-base-dir", cfg.ChrootBase,
		"--netns", cfg.Slot.NetnsPath(),
		"--cgroup-version", "2",
		"--parent-cgroup", "pilots",
	}
	for _, c := range cgroupArgs(cfg.Limits) {
		args = append(args, "--cgroup", c)
	}
	return append(args, "--", "--api-sock", "/run/fc.sock")
}

func fetchIfAbsent(ctx context.Context, dl Uploader, key, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	return dl.GetToFile(ctx, key, dest)
}

func isMissing(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) ||
		strings.Contains(err.Error(), "not found") ||
		strings.Contains(err.Error(), "NoSuchKey"))
}

// pokeGuestClock sets the restored guest's wall clock to now.
//
// CLOCK_REALTIME is frozen at the moment the snapshot was taken; kvm-clock
// keeps CLOCK_MONOTONIC honest but nothing corrects the wall clock. Until this
// lands the machine looks healthy and behaves badly.
func pokeGuestClock(slot *netns.Slot, token, machineID string) {
	deadline := time.Now().Add(initDeadline)
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + slot.AgentAddr() + "/init"

	for time.Now().Before(deadline) {
		body := strings.NewReader(
			fmt.Sprintf(`{"timestamp_nanos":%d}`, time.Now().UnixNano()))

		req, err := http.NewRequest(http.MethodPost, url, body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(initRetryInterval)
	}
	slog.Error("guest clock was never corrected after restore; its wall clock is "+
		"stuck at snapshot time", "machine", machineID)
}
