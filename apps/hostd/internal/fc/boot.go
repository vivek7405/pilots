package fc

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// BakedRootfsPath is where the guest kernel expects its root device to be, as
// recorded in the boot arguments and therefore inside every snapshot.
//
// It is a constant on purpose. Each machine gets its own file bind-mounted at
// this path inside its jail, so a snapshot taken on one host restores on any
// other. Sharing one rootfs file between machines instead produces post-resume
// workqueue lockups in the guest.
const BakedRootfsPath = "/srv/pilots/rootfs.ext4"

// BootArgs is the guest kernel command line.
//
// Do not add nohz=off, tsc=reliable or audit=0. They look like tuning, but the
// pinned kernel is built NO_HZ_IDLE and they actively work against it; the
// predecessor tried them and removed them again.
//
// The ip= addresses match the golden rootfs's static network config exactly
// and are identical for every machine on every host, which is what keeps a
// snapshot host-agnostic.
const BootArgs = "console=ttyS0 reboot=k panic=1 pci=off ro root=/dev/vda " +
	"clocksource=kvm-clock random.trust_cpu=on i8042.nokbd i8042.noaux " +
	"ipv6.disable=0 ipv6.autoconf=1 " +
	"ip=" + netns.TapGuestIP + "::" + netns.TapHostIP + ":255.255.255.252:instance:eth0:off:"

// bootArgsFor renders the guest command line for one machine.
//
// The kernel decides what PID 1 is, so an image that ships its own init is
// overridden here rather than inside the image. Rewriting /sbin/init in the
// image cannot work: the build's fixups are appended to its tar, and GNU tar
// keeps the FIRST symlink entry for a path and silently ignores a later one,
// with or without --overwrite.
func bootArgsFor(cfg Config) string {
	if cfg.InitPath == "" {
		return BootArgs
	}
	return BootArgs + " init=" + cfg.InitPath
}

// Limits are the cgroup v2 constraints applied to a machine.
//
// These are not optional. Firecracker isolates the guest from the host kernel,
// but nothing stops a guest from consuming every core or all remaining memory
// and starving its neighbours -- including hostd itself.
type Limits struct {
	CPUMax  string // e.g. "200000 100000" == 2 cores
	MemMaxB int64
	PidsMax int
}

// Config describes one machine to boot.
type Config struct {
	MachineID string
	Slot      *netns.Slot
	MAC       string

	VCPUs  int
	MemMiB int

	KernelPath     string
	TemplateRootfs string
	CPUTemplate    string
	// InitPath overrides what the kernel runs as PID 1, for an image that
	// ships an init of its own. Empty for the golden template, whose /sbin/init
	// is already what it should be.
	InitPath string

	FirecrackerBin string
	JailerBin      string
	ChrootBase     string
	StateDir       string // /var/lib/pilots/machines/<id>

	// VolumeImage is the host path of this machine's persistent volume image,
	// on the JuiceFS mount that makes its writes durable. Empty for a machine
	// with no volume, which is most of them. It is bind-mounted onto
	// BakedVolumePath inside the jail rather than copied, so the guest writes
	// through to object storage.
	VolumeImage string

	JailUID int
	JailGID int
	Limits  Limits
}

// Machine is a running Firecracker process and everything needed to reach,
// snapshot, or kill it.
type Machine struct {
	ID        string
	Slot      *netns.Slot
	Cmd       *exec.Cmd
	Client    *Client
	ChrootDir string
	StateDir  string
	SerialLog string
	StartedAt time.Time

	// NBD and Uffd serve the machine's disk and memory. They are separate
	// processes, and they outlive hostd on purpose -- see instant.go. Nil for
	// a machine booted from a template file rather than restored.
	NBD  *nbd.Process
	Uffd *uffd.Process

	// captureDone is closed when the background half of the previous snapshot
	// finishes. See awaitCapture.
	captureMu   sync.Mutex
	captureDone chan struct{}
}

// GenerateMAC returns a locally administered unicast address.
func GenerateMAC() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("fc: generate mac: %w", err)
	}
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4]), nil
}

// ChrootDir is where the jailer places this machine's root.
func ChrootDir(base, firecrackerBin, machineID string) string {
	return filepath.Join(base, filepath.Base(firecrackerBin), machineID, "root")
}

// checkChrootBaseUsable rejects a chroot base that cannot host device nodes.
//
// The jailer creates /dev/net/tun, /dev/kvm and /dev/userfaultfd inside the
// chroot with mknod. On a filesystem mounted `nodev` -- which /tmp is on most
// distributions -- mknod SUCCEEDS and the device node looks perfectly normal,
// but every open() of it returns EACCES. Firecracker then fails with:
//
//	Could not create the network device: Open tap device failed:
//	Couldn't open /dev/net/tun: Permission denied (os error 13)
//
// which points at permissions and is thoroughly misleading: it happens as
// uid 0, with all capabilities, against a root-owned 0600 node. Catch it here
// with an error that names the real cause.
func checkChrootBaseUsable(base string) error {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fmt.Errorf("fc: mkdir chroot base %s: %w", base, err)
	}

	var st unix.Statfs_t
	if err := unix.Statfs(base, &st); err != nil {
		return fmt.Errorf("fc: statfs %s: %w", base, err)
	}
	if st.Flags&unix.ST_NODEV != 0 {
		return fmt.Errorf(
			"fc: chroot base %s is on a filesystem mounted nodev; the jailer can create "+
				"device nodes there but firecracker cannot open them, and the failure "+
				"surfaces as an unrelated permission error. Put it on a normal disk "+
				"filesystem, e.g. /var/lib/pilots/jailer", base)
	}
	return nil
}

// prepareJail builds the chroot the jailer will drop into: the per-machine
// rootfs at the constant baked path, and the kernel.
//
// The rootfs is a reflink copy of the golden template where the filesystem
// supports it, so a new machine costs no data copy and diverges lazily as it
// writes.
func prepareJail(cfg Config) (chrootDir string, err error) {
	if err := checkChrootBaseUsable(cfg.ChrootBase); err != nil {
		return "", err
	}
	chrootDir = ChrootDir(cfg.ChrootBase, cfg.FirecrackerBin, cfg.MachineID)

	for _, dir := range []string{
		chrootDir,
		filepath.Join(chrootDir, filepath.Dir(BakedRootfsPath)),
		filepath.Join(chrootDir, "run"),
		cfg.StateDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("fc: mkdir %s: %w", dir, err)
		}
	}

	rootfs := filepath.Join(chrootDir, BakedRootfsPath)
	if err := reflinkCopy(cfg.TemplateRootfs, rootfs); err != nil {
		return "", fmt.Errorf("fc: stage rootfs: %w", err)
	}

	if err := stageVolume(chrootDir, cfg.VolumeImage, cfg.JailUID, cfg.JailGID); err != nil {
		return "", err
	}
	// A bind mount left behind by a prepare that then failed keeps the
	// volume's image open, so `juicefs umount` refuses and the volume is
	// pinned to a host that never even started a machine. There is no Machine
	// yet whose Kill would release it, so release it here.
	defer func() {
		if err != nil {
			_ = unstageVolume(chrootDir)
		}
	}()

	kernel := filepath.Join(chrootDir, "vmlinux.bin")
	if err := hardlinkOrCopy(cfg.KernelPath, kernel); err != nil {
		return "", fmt.Errorf("fc: stage kernel: %w", err)
	}

	// Firecracker runs as the jail uid and must own what it writes: the
	// rootfs, its snapshot output, and the API socket's directory.
	for _, path := range []string{chrootDir, rootfs, filepath.Join(chrootDir, "run")} {
		if err := os.Chown(path, cfg.JailUID, cfg.JailGID); err != nil {
			return "", fmt.Errorf("fc: chown %s: %w", path, err)
		}
	}
	return chrootDir, nil
}

// Boot starts a machine from the golden template and waits for it to reach a
// login prompt.
func Boot(ctx context.Context, cfg Config) (*Machine, error) {
	chrootDir, err := prepareJail(cfg)
	if err != nil {
		return nil, err
	}

	serialLog := filepath.Join(cfg.StateDir, "lifecycle.log")
	logFile, err := os.Create(serialLog)
	if err != nil {
		_ = unstageVolume(chrootDir)
		return nil, fmt.Errorf("fc: create serial log: %w", err)
	}
	defer logFile.Close()

	args := []string{
		"--id", cfg.MachineID,
		"--exec-file", cfg.FirecrackerBin,
		"--uid", strconv.Itoa(cfg.JailUID),
		"--gid", strconv.Itoa(cfg.JailGID),
		"--chroot-base-dir", cfg.ChrootBase,
		// The jailer setns()es into this before dropping privileges, so
		// Firecracker never sees the host's network.
		"--netns", cfg.Slot.NetnsPath(),
		"--cgroup-version", "2",
		"--parent-cgroup", "pilots",
	}
	for _, c := range cgroupArgs(cfg.Limits) {
		args = append(args, "--cgroup", c)
	}
	// Everything after -- belongs to Firecracker itself. The socket path is
	// relative to the chroot.
	args = append(args, "--", "--api-sock", "/run/fc.sock")

	// Deliberately NOT --daemonize. It setsid()s and redirects stdio to
	// /dev/null, which costs the serial console -- the boot-readiness signal
	// and the only source for machine logs -- and detaches the process from
	// the Cmd we would otherwise be able to signal.
	//
	// The context is Background, never the caller's request context: a
	// machine must outlive the HTTP request that created it.
	cmd := exec.CommandContext(context.Background(), cfg.JailerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		// Nothing is holding the volume's image but this bind mount, and no
		// Machine exists to release it later.
		_ = unstageVolume(chrootDir)
		return nil, fmt.Errorf("fc: start jailer: %w", err)
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

	if err := m.Client.WaitForSocket(ctx, 10*time.Second); err != nil {
		_ = m.Kill()
		return nil, err
	}

	if err := m.configure(ctx, cfg); err != nil {
		_ = m.Kill()
		return nil, err
	}
	return m, nil
}

func (m *Machine) configure(ctx context.Context, cfg Config) error {
	if err := m.Client.SetMachineConfig(ctx, MachineConfig{
		VCPUCount: cfg.VCPUs, MemSizeMiB: cfg.MemMiB, SMT: false,
		CPUTemplate: cfg.CPUTemplate,
	}); err != nil {
		return err
	}
	if err := m.Client.SetBootSource(ctx, BootSource{
		KernelImagePath: "/vmlinux.bin", BootArgs: bootArgsFor(cfg),
	}); err != nil {
		return err
	}
	if err := m.Client.SetDrive(ctx, Drive{
		DriveID: "rootfs", PathOnHost: BakedRootfsPath,
		IsRootDevice: true, IsReadOnly: false,
	}); err != nil {
		return err
	}
	// The volume, when there is one. Pre-boot and never later: the drive set
	// is baked into every snapshot this machine takes, and there is no
	// documented way to add a drive to a machine being restored.
	if cfg.VolumeImage != "" {
		if err := m.Client.SetDrive(ctx, volumeDrive()); err != nil {
			return err
		}
	}
	if err := m.Client.SetNetworkInterface(ctx, NetworkInterface{
		IfaceID: "eth0", HostDevName: netns.TapName, GuestMAC: cfg.MAC,
	}); err != nil {
		return err
	}
	return m.Client.Start(ctx)
}

func cgroupArgs(l Limits) []string {
	var out []string
	if l.CPUMax != "" {
		out = append(out, "cpu.max="+l.CPUMax)
	}
	if l.MemMaxB > 0 {
		out = append(out, "memory.max="+strconv.FormatInt(l.MemMaxB, 10))
	}
	if l.PidsMax > 0 {
		out = append(out, "pids.max="+strconv.Itoa(l.PidsMax))
	}
	if len(out) == 0 {
		// With --cgroup-version 2 and no --cgroup at all, --parent-cgroup
		// switches meaning: it MOVES the process into an existing cgroup and
		// fails outright if that cgroup has domain controllers enabled. Always
		// pass at least one.
		out = append(out, "pids.max=2048")
	}
	return out
}

func reflinkCopy(src, dst string) error {
	// --reflink=auto shares extents on btrfs/XFS and silently falls back to a
	// full copy elsewhere, so a new machine usually costs no data movement.
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s -> %s: %w: %s", src, dst, err, out)
	}
	return nil
}

func hardlinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	// Different filesystem: fall back to a copy.
	return reflinkCopy(src, dst)
}
