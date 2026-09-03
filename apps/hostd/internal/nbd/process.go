package nbd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/ctlsock"
)

// SubcommandName is the hidden argv[1] that turns hostd into a handler.
//
// One binary, re-executed, rather than a second artifact to build, ship,
// version, and find at runtime. The predecessor shipped a separate binary and
// located it by guessing at two hard-coded paths.
const SubcommandName = "nbd-handler"

// stopGrace bounds the wait for a handler to exit after being asked to.
const stopGrace = 5 * time.Second

// Process is a running handler, from hostd's side.
type Process struct {
	cmd     *exec.Cmd
	pid     int
	pool    *DevicePool
	control string

	Device string
	Index  int
}

// StartOptions describes a handler to launch.
type StartOptions struct {
	Config
	// Env is passed through to the child, which needs the PILOT_S3_*
	// credentials to open a remote build.
	Env []string
	// LogFile receives the handler's stderr. Without it a handler's failures
	// are invisible: it is not attached to a terminal and hostd's own log is a
	// different process's stream.
	LogFile *os.File
}

// Start launches a handler process and returns once the device is online.
//
// The handler is a separate process on purpose. Firecracker blocks in
// uninterruptible sleep on a device whose server has gone away, so a handler
// living inside hostd would make every hostd restart an outage for every
// running machine -- and hostd's own SIGTERM path deliberately leaves machines
// running to be re-adopted.
func Start(ctx context.Context, pool *DevicePool, opts StartOptions) (*Process, error) {
	device, index, err := pool.Acquire()
	if err != nil {
		return nil, err
	}
	opts.Device = device
	opts.Index = index

	// A pipe rather than parsing stdout: the child writes one byte and closes,
	// so the read returns the instant the device is attached, and EOF with no
	// byte means the child died before getting there.
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		pool.Release(index)
		return nil, fmt.Errorf("nbd: ready pipe: %w", err)
	}
	defer readEnd.Close()

	self, err := os.Executable()
	if err != nil {
		writeEnd.Close()
		pool.Release(index)
		return nil, fmt.Errorf("nbd: locate self: %w", err)
	}

	cmd := exec.Command(self, argv(opts)...)
	cmd.Env = opts.Env
	cmd.ExtraFiles = []*os.File{writeEnd} // becomes fd 3 in the child
	if opts.LogFile != nil {
		cmd.Stdout = opts.LogFile
		cmd.Stderr = opts.LogFile
	}
	// Its own process group, so a signal aimed at the handler cannot reach
	// hostd, and so the group can be killed as a unit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		writeEnd.Close()
		pool.Release(index)
		return nil, fmt.Errorf("nbd: start handler: %w", err)
	}
	// Our copy of the write end must go, or the read below never sees EOF when
	// the child dies and the wait becomes the full timeout on every failure.
	writeEnd.Close()

	p := &Process{cmd: cmd, pid: cmd.Process.Pid, pool: pool, control: opts.ControlSock,
		Device: device, Index: index}

	if err := waitReady(ctx, readEnd, cmd); err != nil {
		_ = p.Stop()
		return nil, err
	}
	return p, nil
}

// waitReady blocks until the handler signals, dies, or the context expires.
func waitReady(ctx context.Context, readEnd *os.File, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		n, err := readEnd.Read(buf)
		switch {
		case n > 0:
			done <- nil
		case err == nil || errors.Is(err, io.EOF):
			done <- fmt.Errorf("nbd: handler exited before the device came online")
		default:
			done <- fmt.Errorf("nbd: wait for handler: %w", err)
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("nbd: handler did not come online: %w", ctx.Err())
	case <-time.After(readyTimeout):
		return fmt.Errorf("nbd: handler did not come online within %s", readyTimeout)
	}
}

// argv builds the child's command line.
func argv(opts StartOptions) []string {
	args := []string{
		SubcommandName,
		"--device", opts.Device,
		"--index", strconv.Itoa(opts.Index),
		"--cache", opts.CachePath,
		"--control", opts.ControlSock,
		"--ready-fd", "3",
	}
	if opts.TemplateDir != "" {
		args = append(args, "--template-dir", opts.TemplateDir)
	}
	if opts.TemplateBuildID != uuid.Nil {
		args = append(args, "--template-build-id", opts.TemplateBuildID.String())
	}
	if opts.RehydrateBuildID != uuid.Nil {
		args = append(args, "--rehydrate-build-id", opts.RehydrateBuildID.String())
	}
	if opts.CacheRoot != "" {
		args = append(args, "--cache-root", opts.CacheRoot)
	}
	if opts.ReadOnly {
		args = append(args, "--read-only")
	}
	return args
}

// Dirty asks the handler which blocks the machine has written.
//
// The VM must be paused first. A bitmap taken while the guest is writing
// describes a disk state that never existed at any instant, and chunkifying
// against it produces a snapshot with a torn filesystem.
func (p *Process) Dirty() (*roaring.Bitmap, error) {
	payload, err := ctlsock.Request(p.control, cmdDirty)
	if err != nil {
		return nil, err
	}
	return parseDirty(payload)
}

// Stop tears the handler down and returns its device to the pool.
//
// The disconnect comes FIRST and from here, not from the child. A handler
// blocked in NBD_DO_IT never reaches its own cleanup, so a plain SIGTERM
// leaves the device attached to a server that is gone: Firecracker wedges in
// D-state, /dev/nbdN stays unusable, and only a reboot clears it.
func (p *Process) Stop() error {
	var errs []error

	// The guard is the hostility battery's negative control and nothing else:
	// it needs both PILOT_FAULTS=1 and PILOT_FAULT_NBD_SKIP_DISCONNECT=1, so
	// on any host that has not deliberately armed both, the ioctl runs exactly
	// as it always has. See faults.go.
	if !skipDisconnect() {
		if err := DisconnectDevice(p.Index); err != nil {
			errs = append(errs, err)
		}
	}

	if p.pid > 0 {
		_ = syscall.Kill(p.pid, syscall.SIGTERM)
		if err := p.waitExit(); err != nil {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
			_ = p.waitExit()
		}
	}

	// Wait for the kernel to finish before the pool can hand this device to
	// anyone else. Releasing on the handler's exit alone is too early: the
	// teardown ends by zeroing the device's size, so the next machine's
	// handler sets a size that is then wiped, and its restore fails on a
	// sizing timeout that points at the wrong machine entirely.
	if err := WaitDetached(p.Index, 0); err != nil {
		errs = append(errs, err)
	}
	p.pool.Release(p.Index)

	// The socket outlives the process it belonged to.
	if p.control != "" {
		if err := os.Remove(p.control); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitExit blocks until the handler is gone or stopGrace expires.
//
// Two mechanisms, because there are two kinds of handler. A process we spawned
// must be REAPED: after SIGTERM it becomes a zombie, and kill(pid, 0) on a
// zombie succeeds, so polling for its absence waits out the full grace period
// on every single teardown -- five seconds added to every destroy, for a
// process that exited immediately. An adopted handler is not our child, Wait
// on it returns ECHILD, and init reaps it, so there the poll is correct.
func (p *Process) waitExit() error {
	deadline := time.After(stopGrace)

	if p.cmd != nil && p.cmd.Process != nil {
		reaped := make(chan struct{})
		go func() { _, _ = p.cmd.Process.Wait(); close(reaped) }()
		select {
		case <-reaped:
			return nil
		case <-deadline:
			return fmt.Errorf("nbd: handler %d did not exit within %s", p.pid, stopGrace)
		}
	}

	for {
		if syscall.Kill(p.pid, 0) != nil {
			return nil
		}
		select {
		case <-deadline:
			return fmt.Errorf("nbd: handler %d did not exit within %s", p.pid, stopGrace)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Pid reports the handler's process id, for breadcrumbs and reconciliation.
func (p *Process) Pid() int { return p.pid }

// AdoptedProcess rebuilds a handle to a handler that survived a hostd restart.
//
// Handlers outlive hostd by design. On restart the daemon re-adopts them from
// breadcrumbs rather than killing and respawning, which would mean tearing the
// device out from under a running VM.
func AdoptedProcess(pool *DevicePool, pid, index int, control string) *Process {
	pool.Reserve(index)
	return &Process{pid: pid, pool: pool, control: control,
		Device: fmt.Sprintf("/dev/nbd%d", index), Index: index}
}
