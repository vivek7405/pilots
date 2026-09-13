package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
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
//
// # Several processes
//
// A machine runs a NAMED SET of processes rather than one anonymous command.
// The shape is sprites' services surface: a name, a command, an optional
// `needs` list for start ordering, a restart count, and per-process logs.
// Exactly one process owns the app port.
//
// It is a set even when it holds one member, which is the ordinary case: an
// image's CMD becomes the process `app` and nothing about a single-process
// machine changes. What it buys is the case an agent hits constantly, a dev
// server plus a worker plus a database in one machine, where the alternative
// is one shell command holding three children the supervisor cannot see, name,
// restart or read the logs of separately.
//
// Processes may also be registered at RUNTIME, through POST /processes. That
// is not a convenience: the editor mounts, the harness plugins and the MCP
// tools all assume a dev server an agent started is still there after a
// bounce, and a supervisor that only knows what the image declared makes every
// one of those feel broken on the second visit. Runtime registrations are
// written to /etc/pilot/app.d so a cold boot brings them back.

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

// DefaultProcess is the name an image's own CMD runs under. A machine that
// declares nothing else has exactly this one.
const DefaultProcess = "app"

// processSpec is what a process is declared as, by the image or at runtime.
type processSpec struct {
	Name    string            `json:"name"`
	Cmd     string            `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	Needs   []string          `json:"needs,omitempty"`
	Port    bool              `json:"port,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	User    string            `json:"user,omitempty"`
}

// processStatus is what GET /processes answers.
type processStatus struct {
	Name     string   `json:"name"`
	Cmd      string   `json:"cmd"`
	State    string   `json:"state"`
	PID      int      `json:"pid,omitempty"`
	Restarts int      `json:"restarts"`
	Needs    []string `json:"needs,omitempty"`
	Port     bool     `json:"port,omitempty"`
}

// process is one supervised child.
type process struct {
	spec     processSpec
	cmd      *exec.Cmd
	running  bool
	stopping bool
	restarts int
	logs     *logRing
	// gen rises on every deliberate stop or restart, so the keepAlive loop
	// started for an older generation exits instead of resurrecting a process
	// the caller asked to stop. Without it, stop races restart and the machine
	// ends up with two copies of the same worker.
	gen int
}

// supervisor runs the machine's processes when nothing else will.
type supervisor struct {
	mu    sync.Mutex
	procs map[string]*process
}

var appSupervisor = &supervisor{procs: map[string]*process{}}

// start launches the image's single command as the default process. It is the
// shape the create path has always called, kept so nothing about a
// one-process machine changes.
func (s *supervisor) start(cmdline string, env map[string]string) (bool, string) {
	return s.startAll([]processSpec{{
		Name: DefaultProcess, Cmd: cmdline, Env: env, Port: true,
	}})
}

// startAll starts a set of processes in dependency order.
//
// The refusal on an already-running set is the one pilot-app.service's path
// makes, for the same reason: a create is the only time the application
// starts, and a wake that re-execs it would kill the process the guest just
// restored.
func (s *supervisor) startAll(specs []processSpec) (bool, string) {
	s.mu.Lock()
	for _, p := range s.procs {
		if p.running {
			s.mu.Unlock()
			return false, "the application is already running; not restarting it"
		}
	}
	s.mu.Unlock()

	ordered, err := orderByNeeds(specs)
	if err != nil {
		return false, err.Error()
	}
	for _, spec := range ordered {
		if ok, msg := s.startOne(spec); !ok {
			return false, msg
		}
	}
	return true, ""
}

// startOne starts or replaces one named process.
func (s *supervisor) startOne(spec processSpec) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p, ok := s.procs[spec.Name]; ok && p.running {
		return false, "process " + spec.Name + " is already running"
	}
	p := s.procs[spec.Name]
	if p == nil {
		p = &process{logs: newLogRing()}
		s.procs[spec.Name] = p
	}
	p.spec, p.stopping = spec, false
	p.gen++

	c, err := s.build(spec, p.logs)
	if err != nil {
		return false, err.Error()
	}
	if err := c.Start(); err != nil {
		return false, "starting " + spec.Name + ": " + err.Error()
	}
	p.cmd, p.running = c, true

	go s.keepAlive(spec.Name, p.gen)
	return true, ""
}

// build assembles the command exactly as pilot-app.service would run it.
func (s *supervisor) build(spec processSpec, logs *logRing) (*exec.Cmd, error) {
	// /bin/sh -c, matching the unit's ExecStart, so a shell-form command from
	// a Dockerfile behaves identically under both mechanisms.
	c := exec.Command("/bin/sh", "-c", "exec "+spec.Cmd)
	c.Dir = spec.WorkDir
	if c.Dir == "" {
		c.Dir = appWorkDir
	}

	// Captured AND mirrored: the ring answers "show me this process", the
	// console keeps the machine-level log the host already reads.
	//
	// An io.Writer rather than an *os.File, so os/exec owns the pipe and the
	// copying goroutine and closes both at Wait. Handing it a file we opened
	// means closing our copy at exactly the right moment, and closing it one
	// line too early leaves the child writing to a descriptor nobody holds,
	// which is silence rather than an error.
	if logs != nil {
		out := io.MultiWriter(logs, os.Stdout)
		c.Stdout, c.Stderr = out, out
	} else {
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
	}

	c.Env = os.Environ()
	for k, v := range spec.Env {
		c.Env = append(c.Env, k+"="+v)
	}

	user := spec.User
	if user == "" {
		user = appUser
	}
	if user != "" {
		uid, gid, err := lookupUser(user)
		if err != nil {
			// Refuse rather than silently running the application as root.
			// A Dockerfile that says USER meant it, and the difference is not
			// visible from outside until something is exploited.
			return nil, fmt.Errorf("the image asks to run as %q: %w", user, err)
		}
		c.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: gid},
		}
	}
	return c, nil
}

// keepAlive restarts one process when it exits.
//
// Restart-always, matching pilot-app.service, so the two worlds behave the
// same way from outside. A crash-looping application is the caller's problem
// to notice; silently stopping after one exit would make it ours.
func (s *supervisor) keepAlive(name string, gen int) {
	for {
		s.mu.Lock()
		p := s.procs[name]
		if p == nil || p.gen != gen {
			s.mu.Unlock()
			return
		}
		c := p.cmd
		s.mu.Unlock()
		if c == nil {
			return
		}

		err := c.Wait()

		s.mu.Lock()
		p = s.procs[name]
		if p == nil || p.gen != gen || p.stopping {
			// Stopped or restarted deliberately: this generation is over and
			// must not resurrect anything.
			if p != nil && p.gen == gen {
				p.running = false
			}
			s.mu.Unlock()
			return
		}
		spec := p.spec
		p.restarts++
		s.mu.Unlock()

		fmt.Fprintf(os.Stderr, "guest-agent: %s exited (%v); restarting\n", name, err)

		// The same delay pilot-app.service uses. Without it a command that
		// fails immediately -- a typo in CMD, a missing interpreter -- spins
		// the guest's CPU at the speed of fork.
		time.Sleep(time.Second)

		s.mu.Lock()
		p = s.procs[name]
		if p == nil || p.gen != gen || p.stopping {
			s.mu.Unlock()
			return
		}
		next, buildErr := s.build(spec, p.logs)
		if buildErr != nil {
			fmt.Fprintf(os.Stderr, "guest-agent: cannot restart %s: %v\n", name, buildErr)
			p.running = false
			s.mu.Unlock()
			return
		}
		if startErr := next.Start(); startErr != nil {
			fmt.Fprintf(os.Stderr, "guest-agent: cannot restart %s: %v\n", name, startErr)
			p.running = false
			s.mu.Unlock()
			return
		}
		p.cmd = next
		s.mu.Unlock()
	}
}

// stop ends one process and leaves it stopped.
func (s *supervisor) stop(name string) error {
	s.mu.Lock()
	p := s.procs[name]
	if p == nil {
		s.mu.Unlock()
		return fmt.Errorf("no process named %q", name)
	}
	if !p.running {
		s.mu.Unlock()
		return nil
	}
	p.stopping, p.gen = true, p.gen+1
	c := p.cmd
	s.mu.Unlock()

	if c != nil && c.Process != nil {
		_ = c.Process.Signal(syscall.SIGTERM)
	}
	// Reaped by the keepAlive loop's Wait, which sees stopping and exits.
	s.mu.Lock()
	p.running = false
	s.mu.Unlock()
	return nil
}

// restart stops and starts one process, leaving every other one alone. That
// separation is the whole reason processes have names.
func (s *supervisor) restart(name string) error {
	s.mu.Lock()
	p := s.procs[name]
	if p == nil {
		s.mu.Unlock()
		return fmt.Errorf("no process named %q", name)
	}
	spec := p.spec
	s.mu.Unlock()

	if err := s.stop(name); err != nil {
		return err
	}
	if ok, msg := s.startOne(spec); !ok {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// register adds a process at runtime and starts it.
func (s *supervisor) register(spec processSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("a process needs a name")
	}
	if spec.Cmd == "" {
		return fmt.Errorf("a process needs a command")
	}
	if strings.ContainsAny(spec.Name, "/ \t\n") {
		return fmt.Errorf("a process name may not contain a slash or a space")
	}
	s.mu.Lock()
	if p, ok := s.procs[spec.Name]; ok && p.running {
		s.mu.Unlock()
		return fmt.Errorf("process %q is already running", spec.Name)
	}
	s.mu.Unlock()

	if ok, msg := s.startOne(spec); !ok {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// remove stops a process and forgets it.
func (s *supervisor) remove(name string) error {
	if err := s.stop(name); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.procs, name)
	s.mu.Unlock()
	return nil
}

// list reports every known process, in name order so two reads of an
// unchanged machine agree.
func (s *supervisor) list() []processStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]processStatus, 0, len(s.procs))
	for name, p := range s.procs {
		st := processStatus{
			Name: name, Cmd: p.spec.Cmd, State: "stopped",
			Restarts: p.restarts, Needs: p.spec.Needs, Port: p.spec.Port,
		}
		if p.running {
			st.State = "running"
			if p.cmd != nil && p.cmd.Process != nil {
				st.PID = p.cmd.Process.Pid
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// logsOf returns one process's captured output.
func (s *supervisor) logsOf(name string, tail int) ([]byte, error) {
	s.mu.Lock()
	p := s.procs[name]
	s.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("no process named %q", name)
	}
	return p.logs.Tail(tail), nil
}

// isRunning reports whether anything is up. Used by the wake path's refusal,
// and worth having separate from the systemd probe so a test can drive one
// without the other.
func (s *supervisor) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.procs {
		if p.running {
			return true
		}
	}
	return false
}

// orderByNeeds sorts processes so that everything a process needs starts
// before it does.
//
// A cycle is refused rather than broken arbitrarily: a machine whose worker
// needs its database and whose database needs its worker is a declaration
// mistake, and starting them in whatever order a map happened to produce would
// make it a mistake that shows up once a week instead of at the first deploy.
// A dependency on a process that was never declared is refused for the same
// reason, since silently starting it first would hide the typo.
func orderByNeeds(specs []processSpec) ([]processSpec, error) {
	byName := make(map[string]processSpec, len(specs))
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Name == "" {
			return nil, fmt.Errorf("a process needs a name")
		}
		if _, dup := byName[s.Name]; dup {
			return nil, fmt.Errorf("two processes are both named %q", s.Name)
		}
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	// Sorted first, so the order is a property of the declaration rather than
	// of map iteration: two boots of the same machine must start the same way.
	sort.Strings(names)

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(specs))
	out := make([]processSpec, 0, len(specs))

	var visit func(string, []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("these processes need each other in a cycle: %s",
				strings.Join(append(path, name), " -> "))
		}
		spec, ok := byName[name]
		if !ok {
			return fmt.Errorf("process %q needs %q, which is not declared",
				path[len(path)-1], name)
		}
		state[name] = visiting
		needs := append([]string(nil), spec.Needs...)
		sort.Strings(needs)
		for _, need := range needs {
			if err := visit(need, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		out = append(out, spec)
		return nil
	}

	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
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
