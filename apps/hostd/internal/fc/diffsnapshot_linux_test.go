package fc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// This is the measurement issue #22 turns on, kept as a test rather than run
// once by hand.
//
// The question upstream Firecracker's own code cannot answer: a Diff snapshot
// derives its dirty set from mincore, which reports page RESIDENCY of the
// host VMA. When guest memory is served through userfaultfd, that VMA is an
// anonymous mapping this repo's handler populates with UFFDIO_COPY. Residency
// there SHOULD equal "installed by the handler". e2b never exercises the
// combination -- they pair a false track_dirty_pages with Full snapshots
// because their fork supplies a pagemap RPC -- so their code is evidence for
// neither side, and the prior-art note records it as an open question.
//
// The rig deliberately does NOT use the jailer, a network namespace, or the
// content-addressed store. Every one of those adds a way for the test to fail
// for reasons unrelated to the question. Firecracker runs bare, in a temp
// directory, as root.
//
// Correctness is proven by comparison rather than by inspection: after the
// Diff has been merged, a Full snapshot of the SAME paused VM must be
// byte-identical to it. If the merge dropped a dirty page, the two differ at
// exactly that page. That is a stronger check than reading the guest back
// through its agent, and it needs no guest networking at all.
func TestDiffSnapshotOverUffdIsCorrectAndCheap(t *testing.T) {
	kernel, rootfs, fcBin, _ := requireBootEnv(t)

	// Even, because a 2MiB-backed machine requires it and this test should
	// run identically under either page size.
	const memMiB = 512
	const memBytes = int64(memMiB) << 20

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir, err := os.MkdirTemp("/var/lib/pilots", "difftest-")
	if err != nil {
		t.Fatalf("work dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// A private copy of the rootfs: the guest writes to it, and a repeat run
	// must start from the same place.
	disk := filepath.Join(dir, "rootfs.ext4")
	if err := exec.Command("cp", "--reflink=auto", rootfs, disk).Run(); err != nil {
		t.Fatalf("copy rootfs: %v", err)
	}

	// ---- Phase 1: boot, settle, and take the seeding Full snapshot --------

	one := newBareFC(t, ctx, fcBin, filepath.Join(dir, "fc1.sock"), filepath.Join(dir, "fc1.log"))
	if err := one.configure(ctx, memMiB, kernel, disk); err != nil {
		t.Fatalf("configure: %v\n%s", err, tail(filepath.Join(dir, "fc1.log")))
	}
	if err := one.client.Start(ctx); err != nil {
		t.Fatalf("start: %v\n%s", err, tail(filepath.Join(dir, "fc1.log")))
	}
	// The login prompt is the readiness signal, exactly as the boot test uses.
	if err := waitForLog(filepath.Join(dir, "fc1.log"), "login:", 90*time.Second); err != nil {
		t.Fatalf("guest never reached a login prompt: %v\n%s",
			err, tail(filepath.Join(dir, "fc1.log")))
	}

	fullMem := filepath.Join(dir, "mem-full.bin")
	fullSnap := filepath.Join(dir, "snap-full.bin")
	if err := one.client.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := one.client.CreateSnapshot(ctx, SnapshotCreate{
		SnapshotType: SnapshotFull, SnapshotPath: fullSnap, MemFilePath: fullMem,
	}); err != nil {
		t.Fatalf("full snapshot: %v", err)
	}
	one.stop()

	if got := apparentSize(t, fullMem); got != memBytes {
		t.Fatalf("the seeding Full is %d bytes, want %d", got, memBytes)
	}
	fullAllocated := allocatedSize(t, fullMem)
	t.Logf("Full: apparent %d, allocated %d (%.1f%%)",
		memBytes, fullAllocated, 100*float64(fullAllocated)/float64(memBytes))

	// ---- Phase 2: restore through the uffd handler ------------------------

	// The image the handler serves, and the image Firecracker will merge the
	// diff into, are SEPARATE files on purpose. Firecracker writes the second
	// one while the handler is still reading the first, and pointing both at
	// one path would have it rewriting its own source mid-flight.
	source := filepath.Join(dir, "mem-source.bin")
	if err := exec.Command("cp", "--reflink=auto", fullMem, source).Run(); err != nil {
		t.Fatalf("copy source image: %v", err)
	}
	target := filepath.Join(dir, "mem-target.bin")
	if err := exec.Command("cp", "--reflink=auto", fullMem, target).Run(); err != nil {
		t.Fatalf("copy target image: %v", err)
	}

	sock := filepath.Join(dir, "uffd.sock")
	handlerDone := make(chan error, 1)
	handlerCtx, stopHandler := context.WithCancel(ctx)
	defer stopHandler()
	go func() {
		handlerDone <- uffd.Run(handlerCtx, uffd.Config{
			Socket:  sock,
			MemFile: source,
		}, nil)
	}()
	if err := waitForSocketFile(sock, 30*time.Second); err != nil {
		t.Fatalf("handler socket never appeared: %v", err)
	}

	two := newBareFC(t, ctx, fcBin, filepath.Join(dir, "fc2.sock"), filepath.Join(dir, "fc2.log"))
	if err := two.client.LoadSnapshot(ctx, SnapshotLoad{
		SnapshotPath: fullSnap,
		MemBackend:   MemBackend{BackendType: "Uffd", BackendPath: sock},
		ResumeVM:     true,
	}); err != nil {
		t.Fatalf("load snapshot through uffd: %v\n%s\nhandler: %s",
			err, tail(filepath.Join(dir, "fc2.log")), drain(handlerDone))
	}

	// Let the restored guest run, so some pages are genuinely dirtied and the
	// diff is not trivially empty.
	time.Sleep(5 * time.Second)

	// ---- Phase 3: the Diff, merged in place ------------------------------

	// Firecracker merges a Diff into mem_file_path when that file is exactly
	// mem_size_mib. target is a copy of the Full, so it is.
	if got := apparentSize(t, target); got != memBytes {
		t.Fatalf("merge target is %d bytes, want exactly %d", got, memBytes)
	}
	if err := two.client.Pause(ctx); err != nil {
		t.Fatalf("pause for diff: %v", err)
	}
	diffSnap := filepath.Join(dir, "snap-diff.bin")
	diffStart := time.Now()
	if err := two.client.CreateSnapshot(ctx, SnapshotCreate{
		SnapshotType: SnapshotDiff, SnapshotPath: diffSnap, MemFilePath: target,
	}); err != nil {
		t.Fatalf("DIFF SNAPSHOT REFUSED over a uffd-backed region: %v\n%s\n\n"+
			"This is the negative answer to the measurement. Lever 2 is "+
			"dropped and levers 1, 3 and 4 ship without it. Do NOT fall back "+
			"to track_dirty_pages: it forces KVM to 4K page tables and "+
			"cancels the hugepage lever.",
			err, tail(filepath.Join(dir, "fc2.log")))
	}

	// The apparent size must not move: a Diff that overwrote the file rather
	// than merging into it would leave a partial image, and every page it did
	// not write reads back as zeros.
	if got := apparentSize(t, target); got != memBytes {
		t.Errorf("after the Diff the image is %d bytes, want %d -- Firecracker "+
			"overwrote it instead of merging, so untouched pages are now holes",
			got, memBytes)
	}
	diffTook := time.Since(diffStart)
	t.Logf("Diff snapshot took %s", diffTook)

	// ---- Phase 4: is the merged image CORRECT? ---------------------------

	// A Full of the same paused VM. If the merge is right, the two files hold
	// the same bytes; if it dropped a dirty page, they differ at that page.
	checkMem := filepath.Join(dir, "mem-check.bin")
	checkSnap := filepath.Join(dir, "snap-check.bin")
	fullStart := time.Now()
	if err := two.client.CreateSnapshot(ctx, SnapshotCreate{
		SnapshotType: SnapshotFull, SnapshotPath: checkSnap, MemFilePath: checkMem,
	}); err != nil {
		t.Fatalf("verification Full snapshot: %v", err)
	}
	fullTook := time.Since(fullStart)
	t.Logf("Full snapshot of the SAME paused state took %s", fullTook)
	two.stop()
	stopHandler()

	if off, ok := firstDifference(t, target, checkMem); !ok {
		t.Errorf("the merged diff and a Full of the same paused VM differ at "+
			"offset %d (page %d): the merge lost a dirty page, and a machine "+
			"restored from it would read that page as whatever the parent had",
			off, off/4096)
	}

	// ---- The gate: O(dirty), not O(RAM) ----------------------------------

	// Time, not file size. The merge target is a dense, fully allocated image
	// that Firecracker rewrites extents inside, so its allocated size is
	// ~100% before the Diff runs and stays there afterwards no matter how
	// little was written -- the first version of this test asserted on that
	// and was measuring the copy, not the snapshot. What lever 2 actually
	// shortens is the PAUSE, and the two calls compared here are of the same
	// VM at the same paused instant, so nothing but the snapshot type differs.
	t.Logf("pause window: Diff %s against Full %s (%.1fx)",
		diffTook, fullTook, float64(fullTook)/float64(diffTook))
	if diffTook >= fullTook {
		t.Errorf("the Diff took %s against the Full's %s at the same paused "+
			"instant: the pause is still O(RAM), so lever 2 bought nothing",
			diffTook, fullTook)
	}

	// The allocated sizes are logged rather than asserted, because both files
	// are dense by construction. Left in because the number is the first
	// thing anyone reading a failure will want.
	t.Logf("allocated: seeding Full %d, merged Diff %d, verification Full %d",
		fullAllocated, allocatedSize(t, target), allocatedSize(t, checkMem))
}

// bareFC is a Firecracker process with no jailer and no network device.
type bareFC struct {
	t      *testing.T
	cmd    *exec.Cmd
	client *Client
}

func newBareFC(t *testing.T, ctx context.Context, bin, sock, logPath string) *bareFC {
	t.Helper()

	log, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	cmd := exec.Command(bin, "--api-sock", sock)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start firecracker: %v", err)
	}

	f := &bareFC{t: t, cmd: cmd, client: NewClient(sock)}
	t.Cleanup(f.stop)

	if err := f.client.WaitForSocket(ctx, 15*time.Second); err != nil {
		t.Fatalf("firecracker api socket: %v\n%s", err, tail(logPath))
	}
	return f
}

// configure sets the machine up for a boot. No network interface: this rig
// answers a memory question, and a tap device would need a namespace.
func (f *bareFC) configure(ctx context.Context, memMiB int, kernel, disk string) error {
	if err := f.client.SetMachineConfig(ctx, MachineConfig{
		VCPUCount: 1, MemSizeMiB: memMiB, SMT: false,
	}); err != nil {
		return fmt.Errorf("machine-config: %w", err)
	}
	if err := f.client.SetBootSource(ctx, BootSource{
		KernelImagePath: kernel,
		BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off ro root=/dev/vda",
	}); err != nil {
		return fmt.Errorf("boot-source: %w", err)
	}
	if err := f.client.SetDrive(ctx, Drive{
		DriveID: "rootfs", PathOnHost: disk,
		IsRootDevice: true, IsReadOnly: false,
	}); err != nil {
		return fmt.Errorf("drive: %w", err)
	}
	return nil
}

func (f *bareFC) stop() {
	if f.cmd == nil || f.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-f.cmd.Process.Pid, syscall.SIGKILL)
	_, _ = f.cmd.Process.Wait()
	f.cmd = nil
}

func apparentSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// allocatedSize is what the file actually costs on disk. This is the number a
// diff snapshot moves; the apparent size stays at the full memory size.
func allocatedSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat_t for %s", path)
	}
	return st.Blocks * 512
}

// firstDifference reports the offset where two files first differ.
func firstDifference(t *testing.T, a, b string) (int64, bool) {
	t.Helper()
	fa, err := os.Open(a)
	if err != nil {
		t.Fatalf("open %s: %v", a, err)
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		t.Fatalf("open %s: %v", b, err)
	}
	defer fb.Close()

	const chunk = 1 << 20
	bufA := make([]byte, chunk)
	bufB := make([]byte, chunk)
	var off int64
	for {
		na, ea := fa.Read(bufA)
		nb, eb := fb.Read(bufB)
		if na != nb {
			return off, false
		}
		if na == 0 {
			if ea != nil && eb != nil {
				return 0, true // both at EOF, identical throughout
			}
			return off, false
		}
		if !bytes.Equal(bufA[:na], bufB[:nb]) {
			for i := 0; i < na; i++ {
				if bufA[i] != bufB[i] {
					return off + int64(i), false
				}
			}
		}
		off += int64(na)
	}
}

func waitForSocketFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("%s did not appear within %s", path, timeout)
}

func tail(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(raw) > 4000 {
		raw = raw[len(raw)-4000:]
	}
	return string(raw)
}

func drain(ch chan error) string {
	select {
	case err := <-ch:
		return fmt.Sprint(err)
	default:
		return "(still running)"
	}
}
