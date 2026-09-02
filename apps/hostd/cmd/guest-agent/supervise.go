package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Starting the application where there is no systemd to start it with.
//
// pilot-app.service is the golden template's mechanism and works there because
// the template is our image and carries systemd. A machine built from a user's
// Dockerfile carries neither: most real base images (node:alpine, python:slim,
// distroless) contain no init at all, which is why the build path points
// /sbin/init at this binary. On those images `systemctl start` is not a
// failure mode to handle, it is a command that does not exist.
//
// So there are two worlds, and the agent picks by looking rather than by being
// told: if systemd is running, hand it the unit; otherwise supervise the
// process here. The decision is made once, at start, and reported either way,
// because "which mechanism started my app" is the first question when an
// application is not serving.

// appWorkDir and appUser come from the build's start spec when the image
// carries one. Variables rather than parameters because pilot-app.service
// consumes them through the environment file and the supervisor reads them
// directly, so both paths need one place to look.
var (
	appWorkDir string
	appUser    string
)

// systemdIsRunning reports whether PID 1 is systemd.
//
// The check is the presence of its private runtime directory, which systemd
// creates before it starts any unit. Asking `systemctl is-system-running`
// would shell out and, worse, returns non-zero in perfectly ordinary states
// (degraded, starting), so it answers a different question from this one.
var systemdIsRunning = func() bool {
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// supervisor runs the application when nothing else will.
type supervisor struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
}

var appSupervisor = &supervisor{}

// start launches the command and keeps it running.
//
// Restart-always, matching pilot-app.service, so the two worlds behave the
// same way from outside. A crash-looping application is the caller's problem
// to notice; silently stopping after one exit would make it ours.
func (s *supervisor) start(cmdline string, env map[string]string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		// The same refusal pilot-app.service's path makes, for the same
		// reason: a create is the only time the application starts, and a wake
		// that re-execs it would kill the process the guest just restored.
		return false, "the application is already running; not restarting it"
	}

	c, err := s.build(cmdline, env)
	if err != nil {
		return false, err.Error()
	}
	if err := c.Start(); err != nil {
		return false, "starting the application: " + err.Error()
	}
	s.cmd, s.running = c, true

	go s.keepAlive(cmdline, env)
	return true, ""
}

// build assembles the command exactly as pilot-app.service would run it.
func (s *supervisor) build(cmdline string, env map[string]string) (*exec.Cmd, error) {
	// /bin/sh -c, matching the unit's ExecStart, so a shell-form command from
	// a Dockerfile behaves identically under both mechanisms.
	c := exec.Command("/bin/sh", "-c", "exec "+cmdline)
	c.Dir = appWorkDir
	c.Stdout, c.Stderr = os.Stdout, os.Stderr

	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}

	if appUser != "" {
		uid, gid, err := lookupUser(appUser)
		if err != nil {
			// Refuse rather than silently running the application as root.
			// A Dockerfile that says USER meant it, and the difference is not
			// visible from outside until something is exploited.
			return nil, fmt.Errorf("the image asks to run as %q: %w", appUser, err)
		}
		c.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: gid},
		}
	}
	return c, nil
}

// keepAlive restarts the application when it exits.
func (s *supervisor) keepAlive(cmdline string, env map[string]string) {
	for {
		s.mu.Lock()
		c := s.cmd
		s.mu.Unlock()
		if c == nil {
			return
		}

		err := c.Wait()
		fmt.Fprintf(os.Stderr, "guest-agent: the application exited (%v); restarting\n", err)

		// The same delay pilot-app.service uses. Without it a command that
		// fails immediately -- a typo in CMD, a missing interpreter -- spins
		// the guest's CPU at the speed of fork.
		time.Sleep(time.Second)

		next, buildErr := s.build(cmdline, env)
		if buildErr != nil {
			fmt.Fprintf(os.Stderr, "guest-agent: cannot restart the application: %v\n", buildErr)
			s.mu.Lock()
			s.cmd, s.running = nil, false
			s.mu.Unlock()
			return
		}
		if startErr := next.Start(); startErr != nil {
			fmt.Fprintf(os.Stderr, "guest-agent: cannot restart the application: %v\n", startErr)
			s.mu.Lock()
			s.cmd, s.running = nil, false
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		s.cmd = next
		s.mu.Unlock()
	}
}

// isRunning reports whether the supervised application is up. Used by the wake
// path's refusal, and worth having separate from the systemd probe so a test
// can drive one without the other.
func (s *supervisor) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// lookupUser resolves a Dockerfile USER to a uid and gid.
//
// os/user is not usable here: it is cgo-backed by default, and this binary is
// built static and copied into an image whose libc is not the build host's.
// The pure-Go fallback reads the same files, so read them directly and keep
// the failure legible.
//
// Accepts the forms Docker accepts: a name, a uid, or either with a group
// after a colon.
func lookupUser(spec string) (uint32, uint32, error) {
	name, group, _ := strings.Cut(spec, ":")

	uid, gid, err := lookupInFile("/etc/passwd", name, 2, 3)
	if err != nil {
		// A numeric uid need not appear in /etc/passwd at all -- distroless
		// images routinely ship USER 65532 with no passwd entry.
		n, convErr := strconv.ParseUint(name, 10, 32)
		if convErr != nil {
			return 0, 0, err
		}
		uid, gid = uint32(n), uint32(n)
	}

	if group != "" {
		g, _, gErr := lookupInFile("/etc/group", group, 2, 2)
		if gErr != nil {
			n, convErr := strconv.ParseUint(group, 10, 32)
			if convErr != nil {
				return 0, 0, gErr
			}
			g = uint32(n)
		}
		gid = g
	}
	return uid, gid, nil
}

// lookupInFile finds a colon-separated record by name and returns two of its
// fields as ids. Shared by the passwd and group lookups, which differ only in
// which columns they want.
func lookupInFile(path, name string, idCol, gidCol int) (uint32, uint32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) <= gidCol || fields[0] != name {
			continue
		}
		id, err := strconv.ParseUint(fields[idCol], 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %q has an unreadable id: %w", path, name, err)
		}
		gid, err := strconv.ParseUint(fields[gidCol], 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %q has an unreadable group: %w", path, name, err)
		}
		return uint32(id), uint32(gid), nil
	}
	return 0, 0, fmt.Errorf("%s has no entry for %q", path, name)
}
