package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/ctlsock"
)

// SubcommandName is the hidden argv[1] that turns hostd into a memory handler.
const SubcommandName = "uffd-handler"

const (
	// readyTimeout bounds the wait for the handler's socket to appear.
	readyTimeout = 15 * time.Second
	// stopGrace bounds the wait for a handler to exit after being asked to.
	stopGrace = 5 * time.Second
)

// Process is a running handler, from hostd's side.
type Process struct {
	cmd *exec.Cmd
	pid int

	// Socket is what goes into Firecracker's snapshot load request.
	Socket string
	// control is where this handler answers hostd's requests.
	control string
}

// StartOptions describes a handler to launch.
type StartOptions struct {
	Config
	// Env is passed through; the child needs PILOT_S3_* to open a build.
	Env []string
	// LogFile receives the handler's output. Without it the handler's
	// failures are invisible -- it is a different process from hostd, so its
	// stderr goes nowhere by default.
	LogFile *os.File
}

// Start launches a handler and returns once its socket is listening.
//
// Returning any earlier would be a race with Firecracker: a snapshot load that
// reaches the socket before it exists fails outright, and the failure names
// the socket rather than the ordering.
//
// A separate process, like the disk handler, and for a stronger reason: this
// one owns the guest's memory. If it dies, the guest's next fault never
// resolves and the VM hangs forever in an uninterruptible wait. Keeping it out
// of hostd is what lets hostd be restarted at all.
func Start(ctx context.Context, opts StartOptions) (*Process, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("uffd: ready pipe: %w", err)
	}
	defer readEnd.Close()

	self, err := os.Executable()
	if err != nil {
		writeEnd.Close()
		return nil, fmt.Errorf("uffd: locate self: %w", err)
	}

	cmd := exec.Command(self, argv(opts)...)
	cmd.Env = opts.Env
	cmd.ExtraFiles = []*os.File{writeEnd} // fd 3 in the child
	if opts.LogFile != nil {
		cmd.Stdout = opts.LogFile
		cmd.Stderr = opts.LogFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		writeEnd.Close()
		return nil, fmt.Errorf("uffd: start handler: %w", err)
	}
	// Our copy must go, or the read below never sees EOF when the child dies
	// and every failure costs the full timeout.
	writeEnd.Close()

	p := &Process{cmd: cmd, pid: cmd.Process.Pid, Socket: opts.Socket, control: opts.ControlSock}
	if err := p.waitReady(ctx, readEnd); err != nil {
		_ = p.Stop()
		return nil, err
	}
	return p, nil
}

func (p *Process) waitReady(ctx context.Context, readEnd *os.File) error {
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		n, err := readEnd.Read(buf)
		switch {
		case n > 0:
			done <- nil
		case err == nil || errors.Is(err, io.EOF):
			done <- errors.New("uffd: handler exited before its socket was listening")
		default:
			done <- fmt.Errorf("uffd: wait for handler: %w", err)
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("uffd: handler did not come up: %w", ctx.Err())
	case <-time.After(readyTimeout):
		return fmt.Errorf("uffd: handler did not come up within %s", readyTimeout)
	}
}

func argv(opts StartOptions) []string {
	args := []string{
		SubcommandName,
		"--socket", opts.Socket,
		"--ready-fd", "3",
	}
	if opts.MemFile != "" {
		args = append(args, "--memfile", opts.MemFile)
	}
	if opts.BuildID != uuid.Nil {
		args = append(args, "--build-id", opts.BuildID.String())
	}
	if opts.ParentBuildID != uuid.Nil {
		args = append(args, "--parent-build-id", opts.ParentBuildID.String())
	}
	if opts.CacheRoot != "" {
		args = append(args, "--cache-root", opts.CacheRoot)
	}
	if opts.PrefetchFile != "" {
		args = append(args, "--prefetch", opts.PrefetchFile)
	}
	if opts.ControlSock != "" {
		args = append(args, "--control", opts.ControlSock)
	}
	return args
}

// Stop tears the handler down.
//
// Called only AFTER Firecracker is gone. A handler killed while the guest is
// running leaves the next fault unanswerable, and the VM blocks in an
// uninterruptible wait that no signal can clear.
func (p *Process) Stop() error {
	var errs []error

	if p.pid > 0 {
		_ = syscall.Kill(p.pid, syscall.SIGTERM)
		if err := p.waitExit(); err != nil {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
			_ = p.waitExit()
		}
	}

	for _, path := range []string{p.Socket, p.control} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitExit blocks until the handler is gone or stopGrace expires.
//
// A process we spawned must be REAPED: after SIGTERM it is a zombie, and
// kill(pid, 0) on a zombie succeeds, so polling for its absence waits out the
// whole grace period on a handler that exited immediately. An adopted handler
// is not our child -- Wait returns ECHILD and init reaps it -- so there the
// poll is the correct mechanism.
func (p *Process) waitExit() error {
	deadline := time.After(stopGrace)

	if p.cmd != nil && p.cmd.Process != nil {
		reaped := make(chan struct{})
		go func() { _, _ = p.cmd.Process.Wait(); close(reaped) }()
		select {
		case <-reaped:
			return nil
		case <-deadline:
			return fmt.Errorf("uffd: handler %d did not exit within %s", p.pid, stopGrace)
		}
	}

	for {
		if syscall.Kill(p.pid, 0) != nil {
			return nil
		}
		select {
		case <-deadline:
			return fmt.Errorf("uffd: handler %d did not exit within %s", p.pid, stopGrace)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// MakeResident installs every page of the machine's memory.
//
// Called BEFORE pausing for a snapshot, never after. Firecracker reads all of
// guest memory to write one, and any page not already resident faults through
// the handler while the guest is frozen -- which is the whole of a first
// checkpoint's freeze.
func (p *Process) MakeResident() error {
	if p.control == "" {
		return nil
	}
	_, err := ctlsock.Request(p.control, cmdPrefault)
	return err
}

func (p *Process) Pid() int { return p.pid }

// AdoptedProcess rebuilds a handle to a handler that survived a hostd restart.
func AdoptedProcess(pid int, socket, control string) *Process {
	return &Process{pid: pid, Socket: socket, control: control}
}
