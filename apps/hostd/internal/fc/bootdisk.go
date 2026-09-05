package fc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// BootFromDisk boots a kernel against a machine's OWN disk chain, served over
// NBD, rather than restoring its memory image. It is what a machine gets on a
// host whose CPU vendor cannot restore the image: the disk is vendor-free, the
// memory is not.
//
// It composes RestoreInstant's disk half with Boot's kernel half, because
// neither does both. Boot cannot take an NBD-backed drive: prepareJail
// reflink-copies the template rootfs to the baked path, which would discard
// every write the machine ever made. RestoreInstant cannot boot: it loads a
// vmstate. What CAN take a device is Firecracker itself -- configure declares
// the drive as the baked path and does not care whether that path is a file or
// a block node, which is exactly how a restore already works.
//
// No uffd handler and no vmstate: the guest's memory comes from a kernel boot.
func BootFromDisk(ctx context.Context, cfg InstantConfig, store block.ObjectStore,
	pool *nbd.DevicePool) (m *Machine, err error) {

	if err := checkChrootBaseUsable(cfg.ChrootBase); err != nil {
		return nil, err
	}

	chrootDir := ChrootDir(cfg.ChrootBase, cfg.FirecrackerBin, cfg.MachineID)
	for _, dir := range []string{
		chrootDir,
		filepath.Join(chrootDir, filepath.Dir(BakedRootfsPath)),
		filepath.Join(chrootDir, "run"),
		cfg.StateDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("fc: mkdir %s: %w", dir, err)
		}
	}

	logFile, err := os.OpenFile(filepath.Join(cfg.StateDir, "handlers.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fc: open handler log: %w", err)
	}
	defer logFile.Close()

	var nbdProc *nbd.Process
	// Anything already started has to be cleaned up if a later step fails, or
	// the host leaks an attached device and a live handler per failed boot.
	defer func() {
		if err == nil {
			return
		}
		if nbdProc != nil {
			_ = nbdProc.Stop()
		}
		_ = unstageVolume(chrootDir)
	}()

	err = inParallel(
		func() error {
			// Teardown-first, so re-creating an existing namespace is safe.
			if err := netns.Setup(cfg.Slot, cfg.MAC, cfg.JailUID); err != nil {
				return fmt.Errorf("fc: netns for a cold boot: %w", err)
			}
			return nil
		},
		func() error {
			var perr error
			nbdProc, perr = nbd.Start(ctx, pool, nbd.StartOptions{
				Config: nbd.Config{
					TemplateDir:      cfg.RootfsTemplateDir,
					TemplateBuildID:  cfg.RootfsTemplateID,
					RehydrateBuildID: cfg.RootfsDiffID,
					CachePath:        CowPath(cfg.StateDir),
					ControlSock:      nbd.ControlSockFor(cfg.StateDir),
					CacheRoot:        cfg.Backends.CacheRoot,
				},
				Env: cfg.Env, LogFile: logFile,
			})
			return perr
		},
	)
	if err != nil {
		return nil, err
	}

	if err = nbd.WaitReady(nbdProc.Index, 0); err != nil {
		return nil, err
	}

	// The disk is a device node at the same baked path a boot's rootfs file
	// occupies, so the guest's fstab and the kernel command line are unchanged.
	jailRootfs := filepath.Join(chrootDir, BakedRootfsPath)
	if err = mknodBlockDevice(nbdProc.Device, jailRootfs, cfg.JailUID, cfg.JailGID); err != nil {
		return nil, err
	}
	if err = stageVolume(chrootDir, cfg.VolumeImage, cfg.JailUID, cfg.JailGID); err != nil {
		return nil, err
	}

	kernel := filepath.Join(chrootDir, "vmlinux.bin")
	if err = hardlinkOrCopy(cfg.KernelPath, kernel); err != nil {
		return nil, fmt.Errorf("fc: stage kernel: %w", err)
	}
	// The rootfs is not in this list: mknodBlockDevice already chowned the
	// node, and chowning it again would be the only difference from a boot.
	for _, path := range []string{chrootDir, filepath.Join(chrootDir, "run"),
		filepath.Dir(jailRootfs)} {
		if err = os.Chown(path, cfg.JailUID, cfg.JailGID); err != nil {
			return nil, fmt.Errorf("fc: chown %s: %w", path, err)
		}
	}

	serialLog := filepath.Join(cfg.StateDir, "lifecycle.log")
	serial, err := os.OpenFile(serialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fc: open serial log: %w", err)
	}
	defer serial.Close()

	// Background, never the caller's request context: a machine must outlive
	// the request that brought it up. Deliberately not --daemonize, for the
	// reason Boot gives.
	cmd := exec.CommandContext(context.Background(), cfg.JailerBin, jailerArgs(cfg.Config)...)
	cmd.Stdout = serial
	cmd.Stderr = serial
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("fc: start jailer for a cold boot: %w", err)
	}

	m = &Machine{
		ID:        cfg.MachineID,
		MemMiB:    cfg.MemMiB,
		Slot:      cfg.Slot,
		Cmd:       cmd,
		Client:    NewClient(filepath.Join(chrootDir, "run", "fc.sock")),
		ChrootDir: chrootDir,
		StateDir:  cfg.StateDir,
		SerialLog: serialLog,
		StartedAt: time.Now(),
		// NBD only. Uffd stays nil, which is what Persist, Adopted and
		// Chunkify all key on: the machine's memory came from a kernel, and its
		// next suspend chunkifies the disk through the dirty bitmap exactly as
		// a woken machine's does.
		NBD: nbdProc,
	}
	// From here the machine owns the handler, so the cleanup above must not
	// also stop it.
	nbdProc = nil
	defer func() {
		if err != nil {
			_ = m.Kill()
			m = nil
		}
	}()

	if err = m.Client.WaitForSocket(ctx, 10*time.Second); err != nil {
		return nil, err
	}
	if err = m.configure(ctx, cfg.Config); err != nil {
		return nil, err
	}
	return m, nil
}
