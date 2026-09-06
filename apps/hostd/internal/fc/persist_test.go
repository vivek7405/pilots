package fc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{
		MachineID: "m_1", Pid: 4242, SlotIdx: 7, MAC: "02:00:00:00:00:01",
		ChrootDir: "/srv/jailer/firecracker/m_1/root", NetnsName: "m_1",
		StartedAtNs: time.Now().UnixNano(),
	}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// The pid is stored separately so a liveness check never depends on
	// parsing the state file.
	pid, err := ReadPid(dir)
	if err != nil {
		t.Fatalf("ReadPid: %v", err)
	}
	if pid != want.Pid {
		t.Errorf("ReadPid = %d, want %d", pid, want.Pid)
	}
}

// A crash partway through a direct write would leave a truncated file that
// never parses again, permanently orphaning the machine. The write must be
// atomic, and it must not leave its temp file behind.
func TestWriteStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{MachineID: "m_1", Pid: 1}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile+".tmp")); !os.IsNotExist(err) {
		t.Error("temp file survived the write")
	}

	// Overwriting must fully replace, not merge.
	if err := WriteState(dir, State{MachineID: "m_1", Pid: 2, SlotIdx: 9}); err != nil {
		t.Fatalf("second WriteState: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.Pid != 2 || got.SlotIdx != 9 {
		t.Errorf("overwrite did not take: %+v", got)
	}
}

// While fc.pid exists, reconcile believes the machine is live -- so a destroy
// that fails to remove it means the next hostd start resurrects a machine the
// user deliberately deleted.
func TestClearBreadcrumbsRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{MachineID: "m_1", Pid: 1}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := ClearBreadcrumbs(dir); err != nil {
		t.Fatalf("ClearBreadcrumbs: %v", err)
	}
	for _, f := range []string{pidFile, stateFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived", f)
		}
	}
	// Idempotent: destroy can run twice.
	if err := ClearBreadcrumbs(dir); err != nil {
		t.Errorf("second ClearBreadcrumbs: %v", err)
	}
}

// The pid alone is not enough to adopt a process: pids get recycled, and a
// stale breadcrumb can name a pid the kernel has since given to something
// else. Adopting that would send lifecycle signals to an innocent process.
func TestLiveProcessRejectsNonFirecracker(t *testing.T) {
	// This test binary is alive but is not Firecracker.
	if got := LiveProcess(os.Getpid()); got != 0 {
		t.Errorf("LiveProcess adopted a non-firecracker process (%d)", got)
	}
	if got := LiveProcess(0); got != 0 {
		t.Errorf("LiveProcess(0) = %d, want 0", got)
	}
	if got := LiveProcess(-1); got != 0 {
		t.Errorf("LiveProcess(-1) = %d, want 0", got)
	}
	// A pid that almost certainly does not exist.
	if got := LiveProcess(4194303); got != 0 {
		t.Errorf("LiveProcess on a dead pid = %d, want 0", got)
	}
}

func TestReconcileReportsLiveness(t *testing.T) {
	root := t.TempDir()

	// A machine whose pid is long gone.
	dead := filepath.Join(root, "m_dead")
	if err := WriteState(dead, State{MachineID: "m_dead", Pid: 4194303}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	// A machine whose pid is this test process: alive, but not Firecracker,
	// so it must NOT be adopted.
	notFC := filepath.Join(root, "m_notfc")
	if err := WriteState(notFC, State{MachineID: "m_notfc", Pid: os.Getpid()}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, err := Reconcile(root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2", len(got))
	}
	for _, r := range got {
		if r.Alive {
			t.Errorf("%s reported alive; neither pid is a live firecracker", r.State.MachineID)
		}
	}
}

func TestReconcileOnMissingRootIsEmpty(t *testing.T) {
	got, err := Reconcile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Reconcile on a missing root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d machines, want 0", len(got))
	}
}

// A machine with an unreadable state file but a live pid still has to be
// reported, or a crashed hostd leaks a running Firecracker nobody owns.
func TestReconcileFallsBackToPidFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "m_corrupt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pidFile), []byte("4194303\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Reconcile(root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The pid is dead, so nothing is reported -- but crucially Reconcile did
	// not fail on the corrupt file.
	if len(got) != 0 {
		t.Errorf("got %d, want 0 (dead pid)", len(got))
	}
}

func TestGenerateMACIsLocallyAdministered(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		mac, err := GenerateMAC()
		if err != nil {
			t.Fatalf("GenerateMAC: %v", err)
		}
		if len(mac) != 17 || mac[:3] != "02:" {
			t.Fatalf("mac %q is not a locally administered unicast address", mac)
		}
		if seen[mac] {
			t.Fatalf("duplicate mac %q after %d draws", mac, i)
		}
		seen[mac] = true
	}
}

// With cgroup v2 and no --cgroup at all, --parent-cgroup changes meaning and
// the jailer fails on any cgroup with domain controllers enabled. There must
// always be at least one.
func TestCgroupArgsAlwaysNonEmpty(t *testing.T) {
	if got := cgroupArgs(Limits{}); len(got) == 0 {
		t.Error("no cgroup args for empty limits; the jailer would fail")
	}
	got := cgroupArgs(Limits{CPUMax: "200000 100000", MemMaxB: 536870912, PidsMax: 512})
	if len(got) != 3 {
		t.Errorf("got %v, want cpu.max, memory.max and pids.max", got)
	}
}

// Handlers outlive hostd by design -- that is why they are separate processes
// -- so their details have to survive in the breadcrumbs too. Without them a
// restart adopts the machine but not its block and fault servers: the device
// stays attached with nothing holding a handle to it, destroying the machine
// leaks both processes, and that device is unusable until the host reboots.
func TestBreadcrumbsCarryTheHandlerProcesses(t *testing.T) {
	dir := t.TempDir()

	want := State{
		MachineID: "m-1", Pid: 4242, SlotIdx: 7,
		ChrootDir: "/var/lib/pilots/jailer/firecracker/m-1/root",
		NBDPid:    111, NBDIndex: 3, NBDControl: "/var/lib/pilots/machines/m-1/nbd.sock",
		UffdPid:     222,
		UffdSocket:  "/var/lib/pilots/jailer/firecracker/m-1/root/uffd.sock",
		UffdControl: "/var/lib/pilots/machines/m-1/uffd-ctl.sock",
	}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.NBDPid != want.NBDPid || got.NBDIndex != want.NBDIndex ||
		got.NBDControl != want.NBDControl {
		t.Errorf("block server details did not round-trip: %+v", got)
	}
	if got.UffdPid != want.UffdPid || got.UffdSocket != want.UffdSocket ||
		got.UffdControl != want.UffdControl {
		t.Errorf("fault server details did not round-trip: %+v", got)
	}
}

// Device zero is a legitimate index, so it must survive a round trip rather
// than being indistinguishable from "no handler". The pid is what says whether
// there is one.
func TestBreadcrumbsKeepDeviceZero(t *testing.T) {
	dir := t.TempDir()

	if err := WriteState(dir, State{MachineID: "m-1", Pid: 1, NBDPid: 99, NBDIndex: 0}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.NBDIndex != 0 || got.NBDPid != 99 {
		t.Errorf("index 0 did not round-trip: index=%d pid=%d", got.NBDIndex, got.NBDPid)
	}
}

// An adopted machine must reserve its device, or the pool hands that index to
// a new machine the moment the old handler exits -- while this one still
// believes it owns it.
func TestAdoptReservesTheDevice(t *testing.T) {
	pool := nbd.NewDevicePool(nbd.DefaultMaxDevices)

	// Real stand-in processes rather than this test's own pid: a handler is
	// only re-attached when its cmdline still shows it running the recorded
	// subcommand for this machine's socket, and the shortcut of standing
	// os.Getpid() in for one can only be kept by weakening that check.
	nbdCtl, uffdCtl := "/run/pilots/m-1/nbd.sock", "/run/pilots/m-1/uffd-ctl.sock"
	nbdPid := startStandInHandler(t, nbd.SubcommandName, "--control", nbdCtl)
	uffdPid := startStandInHandler(t, uffd.SubcommandName, "--control", uffdCtl)

	m := Adopted(State{
		MachineID: "m-1", Pid: os.Getpid(),
		NBDPid: nbdPid, NBDIndex: 5, NBDControl: nbdCtl,
		UffdPid: uffdPid, UffdSocket: "/tmp/uffd.sock", UffdControl: uffdCtl,
	}, t.TempDir(), pool)

	if m == nil {
		t.Fatal("Adopted returned nil")
	}
	if m.NBD == nil {
		t.Fatal("the block server was not re-attached")
	}
	if m.Uffd == nil {
		t.Fatal("the fault server was not re-attached")
	}
	if pool.InUse() != 1 {
		t.Errorf("the pool holds %d devices after adopting one, want 1", pool.InUse())
	}
	if m.NBD.Index != 5 {
		t.Errorf("adopted device index %d, want 5", m.NBD.Index)
	}
}

// A machine with no handlers -- there is none in normal operation now, but a
// breadcrumb written by an older build has none -- must adopt cleanly rather
// than reserving device zero for nobody.
func TestAdoptWithoutHandlersReservesNothing(t *testing.T) {
	pool := nbd.NewDevicePool(nbd.DefaultMaxDevices)

	m := Adopted(State{MachineID: "m-1", Pid: os.Getpid()}, t.TempDir(), pool)
	if m == nil {
		t.Fatal("Adopted returned nil")
	}
	if m.NBD != nil || m.Uffd != nil {
		t.Error("handlers were invented for a machine that recorded none")
	}
	if pool.InUse() != 0 {
		t.Errorf("the pool reserved %d devices for a machine with no handler", pool.InUse())
	}
}

// MemMiB is what the snapshot-type decision compares mem.bin against, so a
// breadcrumb without it makes every snapshot after a hostd restart a Full:
// the Diff lever switches off silently, on every machine, until the next
// wake. Persist -- not just WriteState -- has to carry it.
func TestPersistCarriesMemMiB(t *testing.T) {
	m := &Machine{ID: "m-1", MemMiB: 512, StateDir: t.TempDir(),
		ChrootDir: "/var/lib/pilots/jailer/firecracker/m-1/root"}
	if err := m.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	got, err := ReadState(m.StateDir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.MemMiB != 512 {
		t.Errorf("MemMiB = %d after a round trip, want 512", got.MemMiB)
	}
	if Adopted(got, t.TempDir(), nil).MemMiB != 512 {
		t.Errorf("an adopted machine lost its MemMiB")
	}
}

// A dead machine's breadcrumbs must never hand a live, unrelated pid to the
// teardown.
//
// These breadcrumbs are on persistent disk precisely so they survive a reboot,
// so after one their NBDPid and UffdPid name whatever the kernel has since
// handed those numbers to. Cleanup stops a handler with kill(pid, SIGTERM)
// followed by kill(-pid, SIGKILL) on the whole process group and no identity
// check, and AdoptedDead is the FIRST thing hostd does with those numbers on
// the first boot after a reboot. Attaching one would SIGKILL an unrelated
// service's process group and disconnect an nbd index out from under it.
func TestAdoptedDeadRefusesARecycledHandlerPid(t *testing.T) {
	pool := nbd.NewDevicePool(nbd.DefaultMaxDevices)

	// Alive, and emphatically not a pilots handler: this test binary.
	m := AdoptedDead(State{
		MachineID: "m-1", Pid: os.Getpid(),
		NBDPid: os.Getpid(), NBDIndex: 5, NBDControl: "/tmp/nbd.sock",
		UffdPid: os.Getpid(), UffdSocket: "/tmp/uffd.sock",
		UffdControl: "/tmp/uffd-ctl.sock",
	}, t.TempDir(), pool)

	if m == nil {
		t.Fatal("AdoptedDead returned nil")
	}
	if m.NBD != nil {
		t.Error("a pid that is not this machine's block server was attached; " +
			"stopping it would SIGKILL an unrelated process group")
	}
	if m.Uffd != nil {
		t.Error("a pid that is not this machine's fault server was attached; " +
			"stopping it would SIGKILL an unrelated process group")
	}
	// AdoptedProcess reserves the index as a side effect, so the refusal has
	// to happen before it or the device is claimed for a handler that is not
	// there.
	if pool.InUse() != 0 {
		t.Errorf("the pool reserved %d devices for a handler that does not exist", pool.InUse())
	}
	if m.Cmd != nil {
		t.Error("a dead machine's handle carries a Cmd, so something can still signal its pid")
	}
}

// The other direction: a pid that really is running the recorded handler is
// still picked back up. Without this the refusal above would be indiscriminate
// and every restart would leak the handlers it should have stopped.
//
// This process stands in for the handler, matched on its own real cmdline,
// because a pid's argv is the only evidence there is.
func TestHandlerIsAcceptsThePidStillRunningIt(t *testing.T) {
	raw, err := os.ReadFile("/proc/self/cmdline")
	if err != nil {
		t.Fatalf("read our own cmdline: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) < 2 {
		t.Fatalf("this process has argv %v; the fixture needs a subcommand to match on", args)
	}

	if !handlerIs(os.Getpid(), args[1], args[len(args)-1]) {
		t.Errorf("handlerIs rejected the pid actually running argv %v", args)
	}
	// The subcommand alone is not enough: a second machine's handler runs the
	// same one, and its control socket is what tells them apart.
	if handlerIs(os.Getpid(), args[1], "/var/lib/pilots/machines/somebody-else/nbd.sock") {
		t.Error("handlerIs matched on the subcommand alone, so any machine's handler " +
			"would answer for any other machine's breadcrumbs")
	}
	if handlerIs(0, args[1], args[len(args)-1]) {
		t.Error("handlerIs accepted pid 0")
	}
	if handlerIs(999999, args[1], args[len(args)-1]) {
		t.Error("handlerIs accepted a pid that does not exist")
	}
}

// startStandInHandler runs a process whose /proc cmdline is exactly what a
// hostd handler's is, and returns its pid.
//
// argv[1] has to be the bare subcommand, so the shell is given the script by a
// relative name from its own working directory. The trailing ":" keeps the
// shell from tail-exec'ing into sleep, which would replace the argv this
// exists to present.
func startStandInHandler(t *testing.T, subcommand string, args ...string) int {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, subcommand)
	if err := os.WriteFile(script, []byte("sleep 60\n:\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := &exec.Cmd{
		Path: "/bin/sh", Dir: dir,
		Args:        append([]string{"hostd", subcommand}, args...),
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in %s: %v", subcommand, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	// The fixture has to be what it claims before anything is asserted on it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if handlerIs(cmd.Process.Pid, subcommand, args...) {
			return cmd.Process.Pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "cmdline"))
	t.Fatalf("the stand-in %s never presented the right cmdline: %q", subcommand, raw)
	return 0
}

// The live half of reconcile adopts through the same function, so it needs the
// same refusal.
//
// A handler can die and have its pid recycled inside ONE boot, while its
// Firecracker is still running: hostd restarts, adopts the machine as alive,
// and picks up a pid the kernel has since handed to something else. The next
// teardown then runs kill(-pid, SIGKILL) on that process group and
// DisconnectDevice on the index. Narrower trigger than a reboot, identical
// blast radius.
func TestAdoptRefusesAHandlerPidRecycledInsideOneBoot(t *testing.T) {
	pool := nbd.NewDevicePool(nbd.DefaultMaxDevices)

	// The machine's Firecracker is genuinely alive; its block server is not,
	// and something unrelated now holds that number.
	recycled := startStandInHandler(t, "some-unrelated-service", "--config", "/etc/unrelated.conf")

	m := Adopted(State{
		MachineID: "m-1", Pid: os.Getpid(),
		NBDPid: recycled, NBDIndex: 5, NBDControl: "/run/pilots/m-1/nbd.sock",
	}, t.TempDir(), pool)

	if m == nil {
		t.Fatal("Adopted returned nil")
	}
	if m.NBD != nil {
		t.Error("a recycled pid was adopted as this machine's block server; the next " +
			"teardown would SIGKILL an unrelated process group")
	}
	if pool.InUse() != 0 {
		t.Errorf("the pool reserved %d devices for a handler that is gone", pool.InUse())
	}
	// The machine itself is still adopted: its Firecracker is alive and it has
	// to keep its registry entry, its metering and its teardown path.
	if m.ID != "m-1" || m.Cmd == nil || m.Cmd.Process == nil {
		t.Error("the machine was not adopted at all; a dead handler must not cost it its handle")
	}
}
